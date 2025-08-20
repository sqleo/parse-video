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
	"strings"
	"time"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/sqleo/parse-video/parser"
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
		c.HTML(200, "index.tmpl", gin.H{
			"title": "github.com/sqleo/parse-video Demo",
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

		c.JSON(200, jsonRes)
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
