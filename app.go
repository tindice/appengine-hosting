package app

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/appengine/blobstore"
	"google.golang.org/appengine/log"
	"google.golang.org/appengine/urlfetch"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var ErrUnspecified = errors.New("appengine-hosting: unspecified")

var websites = map[string]WebsiteConfiguration{}
var firebase = map[string]FirebaseConfiguration{}

type WebsiteConfiguration struct {
	MainPageSuffix string
	NotFoundPage   string
}

func init() {
	f, err := os.Open("firebase.json")
	if err == nil {
		defer f.Close()
		json.NewDecoder(f).Decode(&firebase)
	}
}

type HandlerContext struct {
	w        http.ResponseWriter
	r        *http.Request
	gcs      *http.Client
	bucket   string
	object   string
	website  WebsiteConfiguration
	firebase FirebaseConfiguration
}

func StaticWebsiteHandler(w http.ResponseWriter, r *http.Request) HttpResult {
	if code := checkMethod(w, r); code != 0 {
		return HttpResult{Status: code}
	}

	ctx := makeContext(w, r)

	if code, location := ctx.getRedirect(); code != 0 {
		return HttpResult{Status: code, Location: location + ctx.getQuery()}
	}

	if ctx.initWebsite() != nil {
		return HttpResult{Status: http.StatusInternalServerError}
	}

	if location := ctx.getCleanURL(); location != "" {
		return HttpResult{Status: http.StatusMovedPermanently, Location: location + ctx.getQuery()}
	}

	res := ctx.getMetadata()

	if res.StatusCode == http.StatusNotFound {
		return ctx.sendNotFound()
	}
	if res.StatusCode != http.StatusOK {
		return HttpResult{Status: res.StatusCode, Message: res.Status}
	}

	etag := res.Header.Get("Etag")
	lastModified := res.Header.Get("Last-Modified")
	code := checkConditions(r, etag, lastModified, true)

	if code == http.StatusNotModified {
		w.Header()["Cache-Control"] = res.Header["Cache-Control"]
	}
	if code != 0 {
		return HttpResult{Status: code}
	}

	w.Header()["Cache-Control"] = res.Header["Cache-Control"]
	w.Header()["Content-Type"] = res.Header["Content-Type"]
	w.Header()["Content-Language"] = res.Header["Content-Language"]
	w.Header()["Content-Disposition"] = res.Header["Content-Disposition"]
	w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))

	ctx.setHeaders()
	if res.Header.Get("x-goog-stored-content-encoding") == "identity" {
		return ctx.sendBlob(etag, lastModified, true)
	}
	return ctx.sendBlobBody()
}

func makeContext(w http.ResponseWriter, r *http.Request) HandlerContext {
	bucket := r.URL.Hostname()
	object := r.URL.EscapedPath()

	if object == "/" {
		object = "/index.html"
	} else {
		log.Debugf(r.Context(), "**** GET %s -- %s", bucket, object)
	}

	return HandlerContext{
		w:        w,
		r:        r,
		bucket:   bucket,
		object:   object,
		website:  websites[bucket],
		firebase: firebase[bucket],
		gcs: &http.Client{
			Transport: &oauth2.Transport{
				Base:   &urlfetch.Transport{Context: r.Context()},
				Source: google.AppEngineTokenSource(r.Context(), "https://www.googleapis.com/auth/devstorage.read_only"),
			},
		},
	}
}

func (ctx *HandlerContext) initWebsite() error {
	if _, ok := websites[ctx.bucket]; ok {
		return nil
	}

	res, err := ctx.gcs.Get("https://storage.googleapis.com/" + ctx.bucket + "?websiteConfig")

	if err != nil {
		log.Errorf(ctx.r.Context(), "GET %s?websiteConfig: %v", ctx.bucket, err)
		return err
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		log.Errorf(ctx.r.Context(), "GET %s?websiteConfig: %s", ctx.bucket, http.StatusText(res.StatusCode))
		return ErrUnspecified
	}

	if err = xml.NewDecoder(res.Body).Decode(&ctx.website); err != nil {
		log.Errorf(ctx.r.Context(), "Decode %s?websiteConfig: %v", ctx.bucket, err)
		return err
	}

	websites[ctx.bucket] = ctx.website
	return nil
}

func (ctx *HandlerContext) getMetadata() *http.Response {
	notFoundPage := "/" + ctx.website.NotFoundPage
	mainPageSuffix := "/" + ctx.website.MainPageSuffix

	if len(ctx.object) <= 1 {
		ctx.object = mainPageSuffix
	}
	if len(ctx.object) <= 1 || ctx.object == notFoundPage {
		if r := ctx.getRewriteMetadata(ctx.firebase.processRewrites(ctx.r.URL.Path)); r != nil {
			return r
		}
		return &http.Response{StatusCode: http.StatusNotFound}
	}
	if ctx.firebase.TrailingSlash != nil {
		ctx.object = strings.TrimRight(ctx.object, "/")
	}

	res, err := ctx.gcs.Head("https://storage.googleapis.com/" + ctx.bucket + ctx.object)

	if err != nil {
		log.Errorf(ctx.r.Context(), "HEAD %s: %v", ctx.bucket+ctx.object, err)
		return &http.Response{StatusCode: http.StatusInternalServerError}
	}
	if res.StatusCode == http.StatusNotFound || strings.HasSuffix(ctx.object, "/") && res.Header.Get("x-goog-stored-content-length") == "0" {
		if r := ctx.getRewriteMetadata(strings.TrimRight(ctx.object, "/") + mainPageSuffix); r != nil {
			return r
		}
		if ctx.firebase.CleanUrls {
			if r := ctx.getRewriteMetadata(strings.TrimRight(ctx.object, "/") + ".html"); r != nil {
				return r
			}
		}
		if r := ctx.getRewriteMetadata(ctx.firebase.processRewrites(ctx.r.URL.Path)); r != nil {
			return r
		}
	}

	return res
}

func (ctx *HandlerContext) getRewriteMetadata(rewrite string) *http.Response {
	if len(rewrite) > 1 && rewrite[0] == '/' && rewrite != ctx.object {
		res, err := ctx.gcs.Head("https://storage.googleapis.com/" + ctx.bucket + rewrite)
		if err != nil {
			log.Errorf(ctx.r.Context(), "HEAD %s: %v", ctx.bucket+ctx.object, err)
			return &http.Response{StatusCode: http.StatusInternalServerError}
		}
		if res.StatusCode != http.StatusNotFound {
			ctx.object = rewrite
			return res
		}
	}
	return nil
}

func (ctx *HandlerContext) getRedirect() (int, string) {
	return ctx.firebase.processRedirects(ctx.r.URL.Path)
}

func (ctx *HandlerContext) getCleanURL() string {
	path := ctx.r.URL.Path

	if ctx.firebase.CleanUrls {
		path = strings.TrimSuffix(path, ctx.website.MainPageSuffix)
		path = strings.TrimSuffix(path, ".html")
	}

	if ctx.firebase.TrailingSlash != nil {
		path = strings.TrimRight(path, "/")
		if *ctx.firebase.TrailingSlash {
			path = path + "/"
		}
	}

	if path != ctx.r.URL.Path {
		return path
	}

	return ""
}

func (ctx *HandlerContext) getQuery() string {
	query := ctx.r.URL.Query()
	if len(query) == 0 {
		return ""
	}
	return "?" + query.Encode()
}

func (ctx *HandlerContext) setHeaders() {
	setHeaders(ctx.w.Header())
	ctx.firebase.processHeaders(ctx.r.URL.Path, ctx.w.Header())
}

func (ctx *HandlerContext) sendBlob(etag string, modified string, mutable bool) HttpResult {
	key, err := blobstore.BlobKeyForFile(ctx.r.Context(), "/gs/"+ctx.bucket+ctx.object)
	if err != nil {
		log.Errorf(ctx.r.Context(), "BlobKeyForFile /gs/%s: %v", ctx.bucket+ctx.object, err)
		return HttpResult{Status: http.StatusInternalServerError}
	}

	if header := ctx.r.Header.Get("Range"); len(header) > 0 {
		condition := ctx.r.Header.Get("If-Range")
		if len(condition) != 0 && condition != etag && condition != modified {
			header = ""
		}
		if mutable {
			header = ""
		}
		ctx.w.Header().Set("X-AppEngine-BlobRange", header)
	}

	ctx.w.Header().Set("X-AppEngine-BlobKey", string(key))
	return HttpResult{}
}

func (ctx *HandlerContext) sendBlobBody() HttpResult {
	res, err := ctx.gcs.Get("https://storage.googleapis.com/" + ctx.bucket + ctx.object)

	if err != nil {
		log.Errorf(ctx.r.Context(), "GET %s: %v", ctx.bucket+ctx.object, err)
		return HttpResult{Status: http.StatusInternalServerError}
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		log.Errorf(ctx.r.Context(), "GET %s: %s", ctx.bucket+ctx.object, http.StatusText(res.StatusCode))
		return HttpResult{Status: http.StatusInternalServerError}
	}

	io.Copy(ctx.w, res.Body)
	return HttpResult{}
}

func (ctx *HandlerContext) sendNotFound() HttpResult {
	notFoundPage := "/" + ctx.website.NotFoundPage

	if len(notFoundPage) <= 1 {
		return HttpResult{Status: http.StatusNotFound}
	}

	res, err := ctx.gcs.Get("https://storage.googleapis.com/" + ctx.bucket + notFoundPage)

	if err != nil {
		log.Errorf(ctx.r.Context(), "GET %s: %v", ctx.bucket+notFoundPage, err)
		return HttpResult{Status: http.StatusInternalServerError}
	}

	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		log.Errorf(ctx.r.Context(), "GET %s: %s", ctx.bucket+notFoundPage, http.StatusText(res.StatusCode))
		return HttpResult{Status: http.StatusInternalServerError}
	}

	ctx.w.Header()["Content-Type"] = res.Header["Content-Type"]
	ctx.w.Header()["Content-Language"] = res.Header["Content-Language"]
	ctx.w.Header()["Content-Disposition"] = res.Header["Content-Disposition"]

	setHeaders(ctx.w.Header())
	ctx.firebase.processHeaders(notFoundPage, ctx.w.Header())
	ctx.w.WriteHeader(http.StatusNotFound)
	io.Copy(ctx.w, res.Body)
	return HttpResult{}
}

func checkConditions(r *http.Request, etag string, lastModified string, mutable bool) int {
	modified, err := http.ParseTime(lastModified)

	if etag == "" || etag[0] != '"' || err != nil {
		log.Errorf(r.Context(), "checkConditions: invalid etag/lastModified")
		return http.StatusInternalServerError
	}

	if matchers, ok := r.Header["If-Match"]; ok {
		match := false
		for _, matcher := range matchers {
			if matcher == "*" || strings.Contains(matcher, etag) && !mutable {
				match = true
				break
			}
		}
		if !match {
			return http.StatusPreconditionFailed
		}
	} else {
		since, err := http.ParseTime(r.Header.Get("If-Unmodified-Since"))
		if err == nil && (modified.After(since) || mutable) {
			return http.StatusPreconditionFailed
		}
	}

	if matchers, ok := r.Header["If-None-Match"]; ok {
		match := false
		for _, matcher := range matchers {
			if matcher == "*" || strings.Contains(matcher, etag) {
				match = true
				break
			}
		}
		if match {
			return http.StatusNotModified
		}
	} else {
		since, err := http.ParseTime(r.Header.Get("If-Modified-Since"))
		if err == nil && !modified.After(since) {
			return http.StatusNotModified
		}
	}

	return 0
}

func checkMethod(w http.ResponseWriter, r *http.Request) int {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		return http.StatusMethodNotAllowed
	}
	return 0
}

func setHeaders(h http.Header) {
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Strict-Transport-Security", "max-age=86400")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Download-Options", "noopen")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("X-XSS-Protection", "1; mode=block")
}
