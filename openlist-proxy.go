package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/sign"
)

type Link struct {
	Url    string      `json:"url"`
	Header http.Header `json:"header"`
}

type LinkResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    Link   `json:"data"`
}

type Result struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

type Json map[string]any

var (
	port        int
	https       bool
	help        bool
	showVersion bool
	disableSign bool

	certFile string
	keyFile  string
	address  string
	token    string

	s       sign.Sign
	version = "dev"
)

// 带超时的 HTTP 客户端（避免长时间阻塞）
// 如果你希望更高吞吐，可继续自定义 Transport（连接池、KeepAlive 等）
var HttpClient = &http.Client{
	Timeout: 120 * time.Second,
}

func init() {
	flag.IntVar(&port, "port", 5243, "the proxy port.")
	flag.BoolVar(&https, "https", false, "use https protocol.")
	flag.BoolVar(&help, "help", false, "show help")
	flag.BoolVar(&showVersion, "version", false, "show version and exit")
	flag.BoolVar(&disableSign, "disable-sign", false, "disable signature verification")
	flag.StringVar(&certFile, "cert", "server.crt", "cert file")
	flag.StringVar(&keyFile, "key", "server.key", "key file")
	flag.StringVar(&address, "address", "", "openlist address")
	flag.StringVar(&token, "token", "", "openlist token")
	flag.Parse()

	if address == "" || token == "" {
		fmt.Println("ERROR: -address 和 -token 参数必须设置")
		flag.Usage()
		os.Exit(1)
	}

	s = sign.NewHMACSign([]byte(token))
}

func errorResponse(w http.ResponseWriter, httpStatus int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(Result{Code: httpStatus, Msg: msg})
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET,HEAD,OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range,Content-Type,Authorization")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length,Content-Range,Accept-Ranges")
}

func getClientUA(r *http.Request) string {
	if ua := r.Header.Get("X-Client-UA"); ua != "" {
		return ua
	}
	return r.Header.Get("User-Agent")
}

func isDirectUA(ua string) bool {
	if ua == "" {
		return false
	}
	ua = strings.ToLower(ua)
	directKeywords := []string{"aria2", "wget", "curl", "idm", "openlist-direct"}
	for _, k := range directKeywords {
		if strings.Contains(ua, k) {
			return true
		}
	}
	return false
}

var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func delHopByHop(h http.Header) {
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
	// 如果 Connection: xxx,yyy 指定了额外 hop-by-hop 头，也应移除
	if c := h.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			h.Del(strings.TrimSpace(f))
		}
	}
}

// 调 OpenList 获取真实链接
func fetchOpenListLink(filePath string) (*LinkResp, error) {
	payload := Json{"path": filePath}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/fs/link", address), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", token)

	res, err := HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var resp LinkResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func downHandle(w http.ResponseWriter, r *http.Request) {
	// 预检请求：直接返回
	if r.Method == http.MethodOptions {
		setCORSHeaders(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 只允许 GET/HEAD（你也可以按需放开）
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		setCORSHeaders(w)
		errorResponse(w, http.StatusMethodNotAllowed, "Only GET/HEAD/OPTIONS are allowed")
		return
	}

	setCORSHeaders(w)

	filePath := r.URL.Path

	// 签名校验（可关闭）
	if !disableSign {
		signValue := r.URL.Query().Get("sign")
		if err := s.Verify(filePath, signValue); err != nil {
			errorResponse(w, http.StatusUnauthorized, err.Error())
			return
		}
	}

	ua := getClientUA(r)
	direct := isDirectUA(ua)

	// 非中国 IP 强制直连（你已有的函数）
	if IsNonChinaRequest(r) {
		fmt.Println("non-CN request direct:", GetClientIP(r))
		direct = true
	}

	// 1) 获取真实 URL
	resp, err := fetchOpenListLink(filePath)
	if err != nil {
		errorResponse(w, http.StatusBadGateway, err.Error())
		return
	}
	if resp.Code != 200 {
		// OpenList 业务错误透传
		errorResponse(w, resp.Code, resp.Message)
		return
	}

	targetURL := resp.Data.Url
	if !strings.HasPrefix(targetURL, "http") {
		targetURL = "http:" + targetURL
	}

	// 2) 直连：302
	fmt.Println("ua:", ua)
	if direct {
		w.Header().Set("Location", targetURL)
		w.WriteHeader(http.StatusFound)
		return
	}

	fmt.Println("proxy to:", targetURL)

	// 3) 代理转发
	upReq, err := http.NewRequest(r.Method, targetURL, nil) // GET/HEAD 无需 body
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 透传请求头（包含 Range / If-Range / UA 等）
	upReq.Header = make(http.Header, len(r.Header))
	maps.Copy(upReq.Header, r.Header)

	// 合并 OpenList 返回 Header（客户端同名优先）
	for k, vv := range resp.Data.Header {
		if upReq.Header.Get(k) != "" {
			continue
		}
		for _, v := range vv {
			upReq.Header.Add(k, v)
		}
	}

	// 清理 hop-by-hop
	delHopByHop(upReq.Header)

	upRes, err := HttpClient.Do(upReq)
	if err != nil {
		errorResponse(w, http.StatusBadGateway, err.Error())
		return
	}
	defer upRes.Body.Close()

	// 写响应头（过滤敏感/不必要头）
	for k, vv := range upRes.Header {
		if strings.EqualFold(k, "Set-Cookie") {
			continue
		}
		// 如果上游也带了 CORS，我们用自己的覆盖
		if strings.EqualFold(k, "Access-Control-Allow-Origin") ||
			strings.EqualFold(k, "Access-Control-Allow-Methods") ||
			strings.EqualFold(k, "Access-Control-Allow-Headers") ||
			strings.EqualFold(k, "Access-Control-Expose-Headers") {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	// 再补一遍 CORS（确保覆盖）
	setCORSHeaders(w)

	// 状态码
	w.WriteHeader(upRes.StatusCode)

	// HEAD 不写 body
	if r.Method == http.MethodHead {
		return
	}

	// 流式转发，避免大文件占内存
	buf := make([]byte, 256*1024)
	if _, err := io.CopyBuffer(w, upRes.Body, buf); err != nil {
		// 通常是客户端断开
		fmt.Println("copy to client error:", err)
		return
	}
}

func main() {
	if help {
		flag.Usage()
		return
	}

	if showVersion {
		fmt.Println("Version:", version)
		return
	}

	fmt.Printf("OpenList-Proxy - %s\n", version)
	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("listen and serve on %s (https=%v)\n", addr, https)

	InitGeoDB("GeoLite2-Country.mmdb")

	srv := http.Server{
		Addr:    addr,
		Handler: http.HandlerFunc(downHandle),
	}

	var err error
	if !https {
		err = srv.ListenAndServe()
	} else {
		err = srv.ListenAndServeTLS(certFile, keyFile)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Printf("failed to start: %s\n", err.Error())
	}
}
