package hermes

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/fetchbot"
	"github.com/PuerkitoBio/goquery"
)

const (
	DefaultUserAgent = "Hermes Bot (github.com/jtaylor32/hermes"
)

// A Runner defines the parameters for running a single instance of Hermes ETL
type Runner struct {
	// The CrawlDelay is the set time for the Runner to abide by.
	CrawlDelay time.Duration

	// The CancelDuration is the set time for the Runner to cancel immediately.
	CancelDuration time.Duration

	// The CancelAtURL is the specific URL that the Runner will cancel on.
	CancelAtURL string

	// The StopDuration is the set time for the Runner to stop at while still processing the remaining links in the queue.
	StopDuration time.Duration

	// The StopAtURL is the specific URL that the Runner will stop on. It will still process the remaining links in the queue.
	StopAtURL string

	// The MemStatsInterval is a set time for when the Runner will output memory statistics to standard output.
	MemStatsInterval time.Duration

	// The UserAgent is the Runner's user agent string name. Be polite and identify yourself for people to see.
	UserAgent string

	// The WorkerIdleTTL keeps a watch for an idle timeout. When the Runner is crawling if it has finished it's total crawl
	// it will exit after this timeout.
	WorkerIdleTTL time.Duration

	// AutoClose will make the Runner terminate and successfully exit after the WorkerIdleTTL if set to true.
	AutoClose bool

	// The URL a reference pointer to a URL type
	URL *url.URL

	// The Tags are the HTML tags you want to scrape with this Runner
	Tags []string

	// If you want to specify how many documents you want to crawl/scrape the Runner will hit you can specify the size here.
	// If you don't have a specific preference you can leave it alone or set it to 0.
	MaximumDocuments int

	// The TopLevelDomain is a toggle to determine if you want to limit the Runner to a specific TLD. (i.e. .com, .edu, .gov, etc.)
	// If it is set to true it will make sure it stays to the URL's specific TLD.
	TopLevelDomain bool

	// The Subdomain is a toggle to determine if you want to limit the Runner to a subdomain of the URL. If it is set to true
	// it will make sure it stays to the host's domain. Think of it like a wildcard -- *.github.com -- anything link that has
	// github.com will be fetched.
	Subdomain bool

	// the ingestionSet is the array of documents that is scraped by the scraper to be sent back for storage.
	ingestionSet []Document

	// Protect access to dup
	mu sync.Mutex
	// Duplicates table
	dup map[string]bool
}

// New returns a default Runner type. These values can be overwritten to whatever
// after initializing the new Runner reference.
func New() *Runner {
	return &Runner{
		CrawlDelay:       1,
		CancelDuration:   60,
		CancelAtURL:      "",
		StopDuration:     60,
		StopAtURL:        "",
		MemStatsInterval: 0,
		UserAgent:        DefaultUserAgent,
		WorkerIdleTTL:    10,
		AutoClose:        true,
		MaximumDocuments: 100,
		TopLevelDomain:   true,
		Subdomain:        true,
	}
}

// func init() {
// 	// Log as JSON instead of the default ASCII formatter.
// 	log.SetFormatter(&log.JSONFormatter{})

// 	// File to output logs to
// 	now := time.Now()
// 	pre := now.Format("2006-01-02")
// 	filename := "./" + pre + "-log.log"
// 	f, err := os.OpenFile(
// 		filename,
// 		os.O_CREATE|os.O_RDWR|os.O_APPEND,
// 		0755,
// 	)
// 	if err != nil {
// 		panic(err)
// 	}

// 	// Output to filename
// 	log.SetOutput(f)

// 	// Output to stdout instead of the default stderr
// 	// log.SetOutput(os.Stdout)

// 	// Only log the warning severity or above.
// 	log.SetLevel(log.InfoLevel)
// }

// Crawl function that will take a url string and start firing out some crawling functions
// it will return true/false based on the url root it starts with.
func (r *Runner) Crawl() ([]Document, error) {
	r.dup = make(map[string]bool)

	if r.MaximumDocuments < 0 {
		return r.ingestionSet, errors.New("you cannot have a negative document size")
	}

	// Create the muxer
	mux := fetchbot.NewMux()

	// Handle all errors the same
	mux.HandleErrors(fetchbot.HandlerFunc(func(ctx *fetchbot.Context, res *http.Response, err error) {
		fmt.Printf("[ERR] %s %s - %s\n", ctx.Cmd.Method(), ctx.Cmd.URL(), err)
	}))

	// Handle GET requests for html responses, to parse the body and enqueue all links as HEAD
	// requests.
	mux.Response().Method("GET").ContentType("text/html").Handler(fetchbot.HandlerFunc(
		func(ctx *fetchbot.Context, res *http.Response, err error) {
			// Process the body to find the links
			doc, err := goquery.NewDocumentFromReader(res.Body)
			if err != nil {
				return
			}
			// Enqueue all links as HEAD requests
			r.enqueueLinks(ctx, doc)
		}))

	// Handle HEAD requests for html responses coming from the source host - we don't want
	// to crawl links from other hosts.
	mux.Response().Method("HEAD").Host(r.URL.Host).ContentType("text/html").Handler(fetchbot.HandlerFunc(
		func(ctx *fetchbot.Context, res *http.Response, err error) {
			if _, err := ctx.Q.SendStringGet(ctx.Cmd.URL().String()); err != nil {
				fmt.Printf("[ERR] %s %s - %s\n", ctx.Cmd.Method(), ctx.Cmd.URL(), err)
			}
		}))

	// Create the Fetcher, handle the logging first, then dispatch to the Muxer
	h := r.scrapeHandler(r.MaximumDocuments, mux)

	if r.StopAtURL != "" || r.CancelAtURL != "" {
		stopURL := r.StopAtURL
		if r.CancelAtURL != "" {
			stopURL = r.CancelAtURL
		}
		h = stopHandler(stopURL, r.CancelAtURL != "", r.scrapeHandler(r.MaximumDocuments, mux))
	}
	f := fetchbot.New(h)

	// set the fetchbots settings from flag parameters
	f.UserAgent = r.UserAgent
	f.CrawlDelay = r.CrawlDelay * time.Second
	f.WorkerIdleTTL = r.WorkerIdleTTL * time.Second
	f.AutoClose = r.AutoClose

	// First mem stat print must be right after creating the fetchbot
	if r.MemStatsInterval > 0 {
		// Print starting stats
		printMemStats(nil)
		// Run at regular intervals
		runMemStats(f, r.MemStatsInterval)
		// On exit, print ending stats after a GC
		defer func() {
			runtime.GC()
			printMemStats(nil)
		}()
	}

	// Start processing
	q := f.Start()

	// if a stop or cancel is requested after some duration, launch the goroutine
	// that will stop or cancel.
	if r.StopDuration*time.Minute > 0 || r.CancelDuration*time.Minute > 0 {
		after := r.StopDuration * time.Minute
		stopFunc := q.Close
		if r.CancelDuration != 0 {
			after = r.CancelDuration * time.Minute
			stopFunc = q.Cancel
		}

		go func() {
			c := time.After(after)
			<-c
			stopFunc()
		}()
	}

	// Enqueue the seed, which is the first entry in the dup map
	r.dup[r.URL.String()] = true
	_, err := q.SendStringGet(r.URL.String())
	if err != nil {
		fmt.Printf("[ERR] GET %s - %s\n", r.URL.String(), err)
	}
	q.Block()

	return r.ingestionSet, nil
}

// stopHandler stops the fetcher if the stopurl is reached. Otherwise it dispatches
// the call to the wrapped Handler.
func stopHandler(stopurl string, cancel bool, wrapped fetchbot.Handler) fetchbot.Handler {
	return fetchbot.HandlerFunc(func(ctx *fetchbot.Context, res *http.Response, err error) {
		if ctx.Cmd.URL().String() == stopurl {
			// generally not a good idea to stop/block from a handler goroutine
			// so do it in a separate goroutine
			go func() {
				if cancel {
					_ = ctx.Q.Cancel()
				} else {
					_ = ctx.Q.Close()
				}
			}()
			return
		}
		wrapped.Handle(ctx, res, err)
	})
}

// scrapeHandler will fire a scraper function on the page if successful response,
// append the scraped document stored for index ingestion
// and dispatches the call to the wrapped Handler.
func (r *Runner) scrapeHandler(n int, wrapped fetchbot.Handler) fetchbot.Handler {
	return fetchbot.HandlerFunc(func(ctx *fetchbot.Context, res *http.Response, err error) {
		if err == nil && len(r.ingestionSet) < n {
			if res.StatusCode == 200 {
				responseDocument, err := scrape(ctx, r.Tags)
				if err != nil {
					fmt.Printf("[ERR] scraping: %v", err)
				}

				r.mu.Lock()
				defer r.mu.Unlock()
				r.ingestionSet = append(r.ingestionSet, responseDocument)
			}
			fmt.Printf("[%d] %s %s - %s\n", res.StatusCode, ctx.Cmd.Method(), ctx.Cmd.URL(), res.Header.Get("Content-Type"))
		} else if len(r.ingestionSet) >= n {
			go func() {
				ctx.Q.Cancel()
			}()
			return
		}
		wrapped.Handle(ctx, res, err)
	})
}

// enqueueLinks will make sure we are adding links to the queue to be processed
// for crawling and scraping. This will pull all of the hrefs within an html
// page. The nature of this function will also check for duplicates that have
// already been crawled and scraped. If they have not been added to the queue
// they will be appended to the queue.
func (r *Runner) enqueueLinks(ctx *fetchbot.Context, doc *goquery.Document) {
	r.mu.Lock()
	defer r.mu.Unlock()

	doc.Find("a[href]").Each(func(i int, s *goquery.Selection) {
		val, exists := s.Attr("href")
		if exists == false {
			fmt.Println("[ERR]: address within the document")
			return
		}

		// Resolve address
		u, err := url.Parse(val)
		if err != nil {
			fmt.Printf("[ERR]: resolve URL %s - %s\n", u, err)
			return
		}

		// check whether or not the link is an email link
		emailCheck := false
		func(s string, emailCheck *bool) {
			if strings.Contains(s, "mailto:") {
				*emailCheck = true
			}
		}(u.String(), &emailCheck)

		if emailCheck == true {
			fmt.Printf("[ERR] Email link - %s\n", u.String())
			return
		}

		fragmentCheck := false
		func(u *url.URL, fragmentCheck *bool) {
			if u.Fragment != "" {
				*fragmentCheck = true
			}
		}(u, &fragmentCheck)

		if fragmentCheck == true {
			fmt.Printf("[ERR] URL with fragment tag - %s\n", u.String())
			return
		}

		// remove the 'www' from the URL so that we have better duplicate detection
		normalizeLink(u)

		// catch the duplicate urls here before trying to add them to the queue
		if !r.dup[u.String()] {
			// tld & subdomain
			if r.TopLevelDomain == true && r.Subdomain == true {
				rootDomain := getDomain(r.URL.Host)
				current := getDomain(u.Host)

				if rootDomain == current {
					if _, err := ctx.Q.SendStringHead(u.String()); err != nil {
						return
					}
					r.dup[u.String()] = true
					if err != nil {
						fmt.Printf("[ERR]: enqueue head %s - %s\n", u, err)
						return
					}
				} else {
					fmt.Printf("catch: out of domain scope -- %s != %s\n", u.Host, r.URL.Host)
				}
			}

			// tld check
			if r.TopLevelDomain == true && r.Subdomain == false {
				rootTLD := getDomain(r.URL.Host)
				current := getTLD(u.Host)

				if rootTLD == current {
					if _, err := ctx.Q.SendStringHead(u.String()); err != nil {
						return
					}
					r.dup[u.String()] = true
					if err != nil {
						fmt.Printf("[ERR]: enqueue head %s - %s\n", u, err)
						return
					}
				}
			}

			// subdomain check
			if r.Subdomain == true && r.TopLevelDomain == false {
				rootDomain := getDomain(r.URL.Host)
				current := getDomain(u.Host)

				if rootDomain == current {
					if _, err := ctx.Q.SendStringHead(u.String()); err != nil {
						return
					}
					r.dup[u.String()] = true
					if err != nil {
						fmt.Printf("[ERR]: enqueue head %s - %s\n", u, err)
						return
					}
				} else {
					fmt.Printf("catch: out of domain scope -- %s != %s\n", u.Host, r.URL.Host)
				}
			}
		}
	})
}

// remove the www from the URL host
func normalizeLink(u *url.URL) {
	s := strings.Split(u.Host, ".")
	if s[0] == "www" {
		u.Host = strings.Join(s[1:], ".")
	}
	return
}

// getDomain will parse a url and return the domain with the tld on it (ie. example.com)
func getDomain(u string) string {
	var root string
	s := strings.Split(u, ".")
	if len(s) == 0 {
		root = u
		return root
	}
	last := len(s) - 1
	if last == 1 {
		root = s[0] + "." + s[last]
		return root
	} else if last > 1 {
		runnerUp := last - 1
		root = s[runnerUp] + "." + s[last]
	}
	return root
}

// getTLD will parse a url type and return the top-level domain (.com, .edu, .gov, etc.)
func getTLD(u string) string {
	var tld string
	s := strings.Split(u, ".")
	if len(s) == 0 {
		tld = u
		return tld
	} else if len(s) > 0 {
		last := len(s) - 1
		tld = s[last]
	}
	tld = u
	return tld
}
