package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/oschwald/geoip2-golang"
)

var (
	geoDB     *geoip2.Reader
	geoDBOnce sync.Once
)

// InitGeoDB 初始化 GeoIP 数据库（只初始化一次）
func InitGeoDB(dbPath string) {
	geoDBOnce.Do(func() {
		db, err := geoip2.Open(dbPath)
		if err != nil {
			fmt.Println("ERROR: 无法加载 GeoIP 数据库:", err)
			os.Exit(1)
		}
		geoDB = db
		fmt.Println("GeoIP database loaded:", dbPath)
	})
}

// GetClientIP 获取真实客户端 IP（支持反代）
//
// 优先级：
//  1. X-Forwarded-For
//  2. X-Real-IP
//  3. RemoteAddr
func GetClientIP(r *http.Request) net.IP {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip := strings.TrimSpace(strings.Split(xff, ",")[0])
		if parsed := net.ParseIP(ip); parsed != nil {
			return parsed
		}
	}

	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		if parsed := net.ParseIP(strings.TrimSpace(xrip)); parsed != nil {
			return parsed
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return net.ParseIP(host)
	}

	return net.ParseIP(r.RemoteAddr)
}

// IsChinaIP 判断 IP 是否中国大陆
//
// 识别失败时返回 true（保守策略）
func IsChinaIP(ip net.IP) bool {
	if ip == nil || geoDB == nil {
		return true
	}

	record, err := geoDB.Country(ip)
	if err != nil {
		return true
	}

	return record.Country.IsoCode == "CN"
}

// IsNonChinaRequest 判断请求是否来自非中国 IP
func IsNonChinaRequest(r *http.Request) bool {
	ip := GetClientIP(r)
	return !IsChinaIP(ip)
}
