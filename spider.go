package wechat_spider

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
)

type spider struct {
	proxy *goproxy.ProxyHttpServer
}

var _spider = newSpider()

func Regist(proc Processor) {
	_spider.Regist(proc)
}

func OnReq(f func(ctx *goproxy.ProxyCtx) (*http.Request, *http.Response)) {
	_spider.OnReq(f)
}

func Run(port string) {
	_spider.Run(port)
}

func newSpider() *spider {
	sp := &spider{}
	sp.proxy = goproxy.NewProxyHttpServer()

	sp.proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	return sp
}

func Header() http.Header {
	return header
}

func (s *spider) Regist(proc Processor) {
	s.proxy.OnResponse().DoFunc(ProxyHandle(proc))
}

func (s *spider) OnReq(f func(ctx *goproxy.ProxyCtx) (*http.Request, *http.Response)) {
	handler := func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		return f(ctx)
	}
	s.proxy.OnRequest().DoFunc(handler)
}

func (s *spider) Run(port string) {
	if rootConfig.Compress {
		s.OnReq(func(ctx *goproxy.ProxyCtx) (req *http.Request, resp *http.Response) {
			host := ctx.Req.URL.Host
			req = ctx.Req
			if !strings.Contains(host, "mp.weixin.qq.com") {
				resp = goproxy.NewResponse(req, "text/html", http.StatusNotFound, "")
			}
			return
		})
	}
	log.Println("server will at port:" + port)
	log.Fatal(http.ListenAndServe(":"+port, s.proxy))
}

var (
	header http.Header //全局缓存wechat header

	rootConfig = &Config{
		Verbose:     false,
		AutoScroll:  false,
		Compress:    true,
		SleepSecond: 3,
	}
)

type Config struct {
	Verbose     bool      // Debug
	AutoScroll  bool      // Auto scroll page to hijack all history articles
	Compress    bool      // To ingore other request just to save the net cost
	SleepSecond int       // Random seconds to sleep
	DateEnd     time.Time // The date of article which crawler will stop
}

func InitConfig(conf *Config) {
	rootConfig = conf
	if rootConfig.SleepSecond < 1 {
		rootConfig.SleepSecond = 3
	}
}

func orPanic(err error) {
	if err != nil {
		panic(err)
	}
}
