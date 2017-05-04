package wechat_spider

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/palantir/stacktrace"
)

type Processor interface {
	ProcessList(resp *http.Response, ctx *goproxy.ProxyCtx) ([]byte, error)
	ProcessDetail(resp *http.Response, ctx *goproxy.ProxyCtx) ([]byte, error)
	ProcessMetrics(resp *http.Response, ctx *goproxy.ProxyCtx) ([]byte, error)
	ProcessPages() error
	Output()
}

type BaseProcessor struct {
	req             *http.Request
	lastId          string
	data            []byte
	urlResults      []*UrlResult
	detailResult    *DetailResult
	historyUrl      string
	biz             string
	lastPublishTime time.Time

	// The index of urls for detail page
	currentIndex int
	checked      bool
	Type         string
}

type (
	UrlResult struct {
		Mid string
		// url
		Url  string
		_URL *url.URL
	}
	DetailResult struct {
		Id         string
		Url        string
		Data       []byte
		Appmsgstat *MsgStat `json:"appmsgstat"`
	}
	MsgStat struct {
		ReadNum     int `json:"read_num"`
		LikeNum     int `json:"like_num"`
		RealReadNum int `json:"real_read_num"`
	}
)

var (
	replacer = strings.NewReplacer(
		"\t", "", " ", "",
		"&quot;", `"`, "&nbsp;", "",
		`\\`, "", "&amp;amp;", "&",
		"&amp;", "&", `\`, "",
	)
	urlRegex            = regexp.MustCompile(`http://mp.weixin.qq.com/s?[^#"',]*`)
	idRegex             = regexp.MustCompile(`"id":(\d+)`)
	publishTimeRegex    = regexp.MustCompile(`publish_time = "([\-\d]+)" \|\| ""`)
	MsgNotFound         = errors.New("MsgLists not found")
	PublishTimeNotFound = errors.New("Cannot parse article publish time")

	TypeList   = "list"
	TypeDetail = "detail"
	TypeMetric = "metric"
)

func NewBaseProcessor() *BaseProcessor {
	return &BaseProcessor{}
}

func (p *BaseProcessor) init(req *http.Request, data []byte) (err error) {
	p.req = req
	p.data = data
	p.currentIndex = -1
	p.biz = req.URL.Query().Get("__biz")
	p.logf("Running a new wechat processor, please wait...")
	return nil
}
func (p *BaseProcessor) ProcessList(resp *http.Response, ctx *goproxy.ProxyCtx) (data []byte, err error) {
	p.Type = TypeList
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(resp.Body); err != nil {
		return
	}
	if err = resp.Body.Close(); err != nil {
		return
	}

	data = buf.Bytes()
	if err = p.init(ctx.Req, data); err != nil {
		return
	}

	if err = p.processMain(); err != nil {
		return
	}
	return
}

func (p *BaseProcessor) ProcessDetail(resp *http.Response, ctx *goproxy.ProxyCtx) (data []byte, err error) {
	p.Type = TypeDetail
	p.req = ctx.Req
	p.currentIndex++
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(resp.Body); err != nil {
		return
	}
	if err = resp.Body.Close(); err != nil {
		return
	}
	data = buf.Bytes()
	p.detailResult = &DetailResult{Id: genId(p.req.URL.String()), Url: p.req.URL.String(), Data: data}
	return
}

func (p *BaseProcessor) ProcessMetrics(resp *http.Response, ctx *goproxy.ProxyCtx) (data []byte, err error) {
	p.Type = TypeMetric
	p.req = ctx.Req

	var buf bytes.Buffer
	if _, err = buf.ReadFrom(resp.Body); err != nil {
		return
	}
	if err = resp.Body.Close(); err != nil {
		return
	}
	data = buf.Bytes()
	detailResult := &DetailResult{}
	e := json.Unmarshal(data, detailResult)
	if e != nil {
		p.logf("error in parsing json %s\n", string(data))
	}
	detailResult.Url = p.req.Referer()
	detailResult.Id = genId(detailResult.Url)
	p.detailResult = detailResult

	return
}

func (p *BaseProcessor) Sleep() {
	ti := rand.Intn(rootConfig.SleepSecond) + 1
	time.Sleep(time.Duration(ti) * time.Second)
}

func (p *BaseProcessor) UrlResults() []*UrlResult {
	return p.urlResults
}

func (p *BaseProcessor) DetailResult() *DetailResult {
	return p.detailResult
}

func (p *BaseProcessor) GetRequest() *http.Request {
	return p.req
}

func (p *BaseProcessor) Output() {
	urls := []string{}
	fmt.Println("result => [")
	for _, r := range p.urlResults {
		urls = append(urls, r.Url)
	}
	fmt.Println(strings.Join(urls, ","))
	fmt.Println("]")
}

//Parse the html
func (p *BaseProcessor) processMain() error {
	p.urlResults = make([]*UrlResult, 0, 100)
	buffer := bytes.NewBuffer(p.data)
	var msgs string
	str, err := buffer.ReadString('\n')
	for err == nil {
		if strings.Contains(str, "msgList = ") {
			msgs = str
			break
		}
		str, err = buffer.ReadString('\n')
	}
	if msgs == "" {
		return stacktrace.Propagate(MsgNotFound, "Failed parse main")
	}
	msgs = replacer.Replace(msgs)
	urls := urlRegex.FindAllString(msgs, -1)
	if len(urls) < 1 {
		return stacktrace.Propagate(MsgNotFound, "Failed find url in  main")
	}
	p.urlResults = make([]*UrlResult, len(urls))
	for i, u := range urls {
		p.urlResults[i] = &UrlResult{Url: u}
	}

	idMatcher := idRegex.FindAllStringSubmatch(msgs, -1)

	if len(idMatcher) < 1 {
		return stacktrace.Propagate(MsgNotFound, "Failed find id in  main")
	}
	p.lastId = idMatcher[len(idMatcher)-1][1]
	return nil
}

func (p *BaseProcessor) ProcessPages() (err error) {
	if err = p.sendCheckurl(); err != nil {
		return
	}
	var pageUrl = p.genPageUrl()
	p.logf("process pages....")
	req, err := http.NewRequest("GET", pageUrl, nil)
	if err != nil {
		return stacktrace.Propagate(err, "Failed new page request")
	}
	for k, _ := range p.req.Header {
		req.Header.Set(k, p.req.Header.Get(k))
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return stacktrace.Propagate(err, "Failed get page response")
	}
	bs, _ := ioutil.ReadAll(resp.Body)
	defer resp.Body.Close()

	str := replacer.Replace(string(bs))
	result := urlRegex.FindAllString(str, -1)
	if len(result) < 1 {
		return stacktrace.Propagate(err, "Failed get page url")
	}
	idMatcher := idRegex.FindAllStringSubmatch(str, -1)
	if len(idMatcher) < 1 {
		return stacktrace.Propagate(err, "Failed get page id")
	}
	lastId := idMatcher[len(idMatcher)-1][1]

	for _, u := range result {
		p.urlResults = append(p.urlResults, &UrlResult{Url: u})
	}

	if rootConfig.DateEnd != nil {
		if lastPublishedAt, err := getArticleTime(p.urlResults[len(p.urlResults)-1].Url); err == nil {
			p.logf("Last crawled article is published at: %s", lastPublishedAt)

			if p.lastPublishTime.Before(rootConfig.DateEnd) {
				p.logf("End date reached, stop crawling")
				return nil
			}
		}
	}

	if lastId != "" {
		if p.lastId == lastId {
			i, _ := strconv.Atoi(lastId)
			p.lastId = fmt.Sprintf("%d", i-10)
		} else {
			p.lastId = lastId
		}
		p.Sleep()
		return p.ProcessPages()
	}
	return nil
}

func (p *BaseProcessor) genPageUrl() string {
	p.logf("loading pages, urls size now is %d", len(p.UrlResults()))
	otherQuery := strings.Replace(p.req.URL.RawQuery, "action=home", "", -1)
	return fmt.Sprintf("https://mp.weixin.qq.com/mp/profile_ext?%s&frommsgid=%s&f=json&count=10&is_ok=1&action=getmsg&f=json&wxtoken=&x5=1&uin=777&key=777", otherQuery, p.lastId)
}

func (p *BaseProcessor) sendCheckurl() (err error) {
	if p.checked {
		return nil
	}
	p.checked = true
	values := url.Values{}
	query := p.req.URL.Query()
	values.Add("__biz", query.Get("__biz"))
	values.Add("scene", query.Get("scene"))
	values.Add("url_list", "")
	urlStr := fmt.Sprintf("http://mp.weixin.qq.com/mp/profile_ext?action=urlcheck&uin=%s&key=%s&pass_ticket=%s", query.Get("uin"), query.Get("key"), query.Get("pass_ticket"))

	req, err := http.NewRequest("POST", urlStr, strings.NewReader(values.Encode()))
	if err != nil {
		return stacktrace.Propagate(err, "Failed check request")
	}
	for k, _ := range p.req.Header {
		req.Header.Set(k, p.req.Header.Get(k))
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func genId(urlStr string) string {
	uri, _ := url.ParseRequestURI(urlStr)
	return hashKey(uri.Query().Get("__biz") + "_" + uri.Query().Get("mid") + "_" + uri.Query().Get("idx"))
}

func getArticleTime(articleUrl string) (*time.Time, error) {
	response, err := http.Get(articleUrl)

	if err != nil {
		return nil, err
	}

	if response.StatusCode != 200 {
		return nil, PublishTimeNotFound
	}

	responseBody, _ := ioutil.ReadAll(response.Body)
	defer response.Body.Close()

	publishedTimeString := publishTimeRegex.FindStringSubmatch(string(responseBody))

	if len(publishedTimeString) < 2 {
		return nil, PublishTimeNotFound
	}

	lastPublishTime, err := time.Parse("2006-01-02", publishedTimeString[1])

	if err != nil {
		return nil, err
	}

	return &lastPublishTime, nil
}

func hashKey(key string) string {
	h := md5.New()
	io.WriteString(h, key)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (P *BaseProcessor) logf(format string, msg ...interface{}) {
	if rootConfig.Verbose {
		Logger.Printf(format, msg...)
	}
}
