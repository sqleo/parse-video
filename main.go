package main

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"time"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/sqleo/parse-video/parser"
	"github.com/sqleo/parse-video/storage"
)

type HttpResponse struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data interface{} `json:"data"`
}

//go:embed templates/*
var files embed.FS

func main() {
	r := gin.Default()

	// 初始化 SQLite 存储
	dbPath := os.Getenv("PARSE_VIDEO_SQLITE_PATH")
	if dbPath == "" {
		dbPath = "data/parse.db"
	}
	if err := storage.Init(dbPath); err != nil {
		log.Fatalf("init sqlite storage failed: %v", err)
	}

	// 根据相关环境变量，确定是否需要使用basic auth中间件验证用户
	if os.Getenv("PARSE_VIDEO_USERNAME") != "" && os.Getenv("PARSE_VIDEO_PASSWORD") != "" {
		r.Use(gin.BasicAuth(gin.Accounts{
			os.Getenv("PARSE_VIDEO_USERNAME"): os.Getenv("PARSE_VIDEO_PASSWORD"),
		}))
	}

	sub, err := fs.Sub(files, "templates")
	if err != nil {
		panic(err)
	}
	tmpl := template.Must(template.ParseFS(sub, "*.tmpl"))
	r.SetHTMLTemplate(tmpl)
	r.GET("/", func(c *gin.Context) {
		// 生成 canonical 等 SEO 字段
		scheme := "http"
		if c.Request.TLS != nil {
			scheme = "https"
		}
		if xfp := c.GetHeader("X-Forwarded-Proto"); xfp != "" {
			scheme = xfp
		}
		canonical := fmt.Sprintf("%s://%s/", scheme, c.Request.Host)

		c.HTML(200, "index.tmpl", gin.H{
			"title":       "在线短视频去水印解析 - 支持抖音、快手、微博等",
			"description": "免费在线短视频去水印解析工具，支持抖音、快手、微博、B站等平台，一键解析并下载无水印视频、封面、音频与图集。",
			"keywords":    "视频解析,去水印,抖音解析,快手解析,无水印下载,短视频下载",
			"site_name":   "视频解析",
			"canonical":   canonical,
			"og_image":    "https://cdn.jsdelivr.net/gh/5ime/img/avatar.jpg",
		})
	})

	// 代理下载，避免跨域与防盗链限制，强制浏览器保存为附件
	r.GET("/download", func(c *gin.Context) {
		raw := c.Query("url")
		if raw == "" {
			c.String(http.StatusBadRequest, "missing url")
			return
		}
		filename := c.Query("filename")

		u, err := url.Parse(raw)
		if err != nil {
			c.String(http.StatusBadRequest, "invalid url")
			return
		}
		if filename == "" {
			base := path.Base(u.Path)
			if base == "" || base == "/" || base == "." {
				base = "download"
			}
			filename = base
		}

		req, _ := http.NewRequest("GET", raw, nil)
		// 统一以 Douyin 来源访问，规避防盗链
		req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 16_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.6 Mobile/15E148 Safari/604.1")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Range", "bytes=0-")
		req.Header.Set("Origin", "https://www.douyin.com")
		req.Header.Set("Referer", "https://www.douyin.com/")
		req.Header.Set("Sec-Fetch-Dest", "video")
		req.Header.Set("Sec-Fetch-Mode", "no-cors")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Host = u.Host

		client := &http.Client{CheckRedirect: func(req2 *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			// 复制首个请求头，避免重定向后 UA/Referer 丢失
			if len(via) > 0 {
				for k, vv := range via[0].Header {
					for _, v := range vv {
						req2.Header.Add(k, v)
					}
				}
			}
			return nil
		}}
		resp, err := client.Do(req)
		if err != nil {
			c.String(http.StatusBadGateway, fmt.Sprintf("fetch failed: %v", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			c.Status(resp.StatusCode)
			_, _ = io.Copy(c.Writer, resp.Body)
			return
		}

		ct := resp.Header.Get("Content-Type")
		if ct != "" {
			c.Header("Content-Type", ct)
		} else {
			ct = "application/octet-stream"
			c.Header("Content-Type", ct)
		}

		// 若无扩展名，依据 Content-Type 追加合理扩展名，便于本地播放器识别
		if !strings.Contains(filename, ".") {
			var ext string
			switch {
			case strings.HasPrefix(ct, "video/mp4"):
				ext = ".mp4"
			case strings.HasPrefix(ct, "audio/mpeg"):
				ext = ".mp3"
			case strings.HasPrefix(ct, "image/jpeg"):
				ext = ".jpg"
			case strings.HasPrefix(ct, "image/png"):
				ext = ".png"
			case strings.Contains(ct, "mpegurl"):
				ext = ".m3u8"
			case strings.Contains(ct, "mp2t"):
				ext = ".ts"
			}
			if ext != "" {
				filename += ext
			}
		}

		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", url.QueryEscape(filename)))
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			c.Header("Content-Length", cl)
		}
		c.Status(http.StatusOK)
		_, _ = io.Copy(c.Writer, resp.Body)
	})

	r.GET("/video/share/url/parse", func(c *gin.Context) {
		paramUrl := c.Query("url")
		parseRes, err := parser.ParseVideoShareUrlByRegexp(paramUrl)
		jsonRes := HttpResponse{
			Code: 200,
			Msg:  "解析成功",
			Data: parseRes,
		}
		if err != nil {
			jsonRes = HttpResponse{
				Code: 201,
				Msg:  err.Error(),
			}
		}

		_ = storage.Append(c.Request.Context(), storage.Record{
			Endpoint: "/video/share/url/parse",
			Input:    storage.Input{ShareURL: paramUrl},
			ClientIP: c.ClientIP(),
			UserAgent: c.GetHeader("User-Agent"),
			Result:   parseRes,
			Error:    func() string { if err != nil { return err.Error() }; return "" }(),
		})

		c.JSON(http.StatusOK, jsonRes)
	})

	r.GET("/video/id/parse", func(c *gin.Context) {
		videoId := c.Query("video_id")
		source := c.Query("source")

		parseRes, err := parser.ParseVideoId(source, videoId)
		jsonRes := HttpResponse{
			Code: 200,
			Msg:  "解析成功",
			Data: parseRes,
		}
		if err != nil {
			jsonRes = HttpResponse{
				Code: 201,
				Msg:  err.Error(),
			}
		}

		_ = storage.Append(c.Request.Context(), storage.Record{
			Endpoint: "/video/id/parse",
			Source:   source,
			Input:    storage.Input{VideoID: videoId},
			ClientIP: c.ClientIP(),
			UserAgent: c.GetHeader("User-Agent"),
			Result:   parseRes,
			Error:    func() string { if err != nil { return err.Error() }; return "" }(),
		})

		c.JSON(200, jsonRes)
	})

	// 查询解析日志
	r.GET("/logs", func(c *gin.Context) {
		parseTime := func(s string) *time.Time {
			if s == "" {
				return nil
			}
			allDigits := true
			for i := range s {
				if s[i] < '0' || s[i] > '9' {
					allDigits = false
					break
				}
			}
			if allDigits {
				if sec, err := time.ParseDuration(s + "s"); err == nil {
					t := time.Unix(int64(sec.Seconds()), 0)
					return &t
				}
			}
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return &t
			}
			return nil
		}

		opts := storage.QueryOptions{
			Start:    parseTime(c.Query("start")),
			End:      parseTime(c.Query("end")),
			Source:   c.Query("source"),
			Endpoint: c.Query("endpoint"),
			Contains: c.Query("contains"),
			ClientIP: c.Query("client_ip"),
		}
		if v := c.Query("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				opts.Limit = n
			}
		}
		if v := c.Query("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				opts.Offset = n
			}
		}

		list, err := storage.Query(c.Request.Context(), opts)
		if err != nil {
			c.JSON(http.StatusOK, HttpResponse{Code: 201, Msg: err.Error()})
			return
		}
		c.JSON(http.StatusOK, HttpResponse{Code: 200, Msg: "ok", Data: list})
	})

	srv := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	go func() {
		// 服务连接
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	// 等待中断信号以优雅地关闭服务器 (设置 5 秒的超时时间)
	quit := make(chan os.Signal)
	signal.Notify(quit, os.Interrupt)
	<-quit
	log.Println("Shutdown Server ...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server Shutdown:", err)
	}
	log.Println("Server exiting")

}
