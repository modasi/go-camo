package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var validReqHeaders = map[string]bool{
	"Accept":            true,
	"Accept-Charset":    true,
	"Accept-Encoding":   true,
	"Cache-Control":     true,
	"If-Modified_Since": true,
}

func noRedirect(req *http.Request, via []*http.Request) error {
	return errors.New("Redirect")
}

func validateURL(path string, key []byte) (surl string, valid bool) {
	pathParts := strings.SplitN(path[1:], "/", 3)
	valid = false
	if len(pathParts) != 2 {
		log.Println("Bad path format", pathParts)
		return
	}
	hexdig, hexurl := pathParts[0], pathParts[1]
	urlBytes, err := hex.DecodeString(hexurl)
	if err != nil {
		log.Println("Bad Hex Decode", hexurl)
		return
	}
	surl = string(urlBytes)
	//log.Println(surl)
	mac := hmac.New(sha1.New, key)
	mac.Write([]byte(surl))
	macSum := hex.EncodeToString(mac.Sum([]byte{}))
	if macSum != hexdig {
		log.Printf("Bad signature: %s != %s", macSum, hexdig)
		return
	}
	valid = true
	return
}

type ProxyHandler struct {
	Transport       *http.Transport
	HMacKey         []byte
	RegexpAllowlist []*regexp.Regexp
	RegexpDenylist  []*regexp.Regexp
}

func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	//log.Println("Request:", req.URL)

	// do fiddly things
	if req.Method != "GET" {
		log.Println("Something other than GET received", req.Method)
		http.Error(w, "Method Not Implemented", http.StatusNotImplemented)
		return
	}

	surl, ok := validateURL(req.URL.Path, p.HMacKey)
	if !ok {
		http.Error(w, "Bad Signature", http.StatusForbidden)
		return
	}
	//log.Println("URL:", surl)

	u, err := url.Parse(surl)
	if err != nil {
		log.Println(err)
		http.Error(w, "Bad url", http.StatusBadRequest)
		return
	}

	if u.Host == "" {
		http.Error(w, "Bad url", http.StatusNotFound)
		return
	}

	// if Allowlist is set, require match
	matchFound := false
	if len(p.RegexpAllowlist) > 0 {
		for _, rgx := range p.RegexpAllowlist {
			if rgx.MatchString(u.Host) {
				matchFound = true
			}
		}
	}
	if !matchFound {
		http.Error(w, "Allowlist host failure", http.StatusNotFound)
		return
	}

	// filter out Denylist urls based on regexes. Do this second
	// as Denylist takes precedence
	for _, rgx := range p.RegexpDenylist {
		if rgx.MatchString(u.Host) {
			http.Error(w, "Denylist host failure", http.StatusNotFound)
			return
		}
	}

	nreq, err := http.NewRequest("GET", surl, nil)
	if err != nil {
		log.Println("Something weird happened")
		http.Error(w, "Error Fetching Resource", http.StatusNotFound)
		return
	}

	// filter headers
	for hdr, val := range req.Header {
		if validReqHeaders[hdr] {
			nreq.Header[hdr] = val
		}
	}
	nreq.Header.Add("connection", "close")
	nreq.Header.Add("user-agent", "pew pew pew")

	resp, err := p.Transport.RoundTrip(nreq)
	if err != nil {
		log.Println("Could not connect to endpoint", err)
		if strings.Contains(err.Error(), "timeout") {
			http.Error(w, "Error Fetching Resource", http.StatusBadGateway)
		} else {
			http.Error(w, "Error Fetching Resource", http.StatusNotFound)
		}
		return
	}
	defer resp.Body.Close()
	defer p.Transport.CloseIdleConnections()

	// check for too large a response
	if resp.ContentLength > 5242880 {
		log.Println("Content length exceeded")
		http.Error(w, "Content length exceeded", http.StatusBadRequest)
		return
	}

	// check for redirects. we do not follow.
	if resp.StatusCode >= 300 && resp.StatusCode <= 307 {
		log.Println("Refusing to follow redirects")
		http.Error(w, "Refusing to follow redirects", http.StatusNotFound)
		return
	}

	// check content type
	ct, ok := resp.Header[http.CanonicalHeaderKey("content-type")]
	if !ok || ct[0][:6] != "image/" {
		log.Println("Non-Image content-type returned")
		http.Error(w, "Non-Image content-type returned", http.StatusBadRequest)
		return
	}

	for hdr, val := range resp.Header {
		h := w.Header()
		h[hdr] = val
	}
	h := w.Header()
	h.Add("X-Content-Type-Options", "nosniff")

	io.Copy(w, resp.Body)
	//log.Println(req, resp.StatusCode)
}

var hmacKeyFlag = flag.String("hmacKey", "", "HMAC Key")
var configFileFlag = flag.String("configFile", "", "JSON Config File")
var bindAddress = flag.String("bindAddress", "0.0.0.0:8080", "Address:Port to bind to")

type configParams struct {
	HmacKey   string
	Allowlist []string
	Denylist  []string
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()

	config := &configParams{}
	if *configFileFlag != "" {
		b, err := ioutil.ReadFile(*configFileFlag)
		if err != nil {
			log.Fatal("Could not read configFile", err)
		}
		err = json.Unmarshal(b, &config)
		if err != nil {
			log.Fatal("Could not parse configFile", err)
		}
	}

	// flags override config file
	if *hmacKeyFlag != "" {
		config.HmacKey = *hmacKeyFlag
	}
	//log.Println("Config", config.HmacKey, config.Allowlist, config.Denylist)

	tr := &http.Transport{
		Dial: func(netw, addr string) (net.Conn, error) {
			// 2 second timeout on requests
			timeout := time.Second * time.Duration(2)
			c, err := net.DialTimeout(netw, addr, timeout)
			if err != nil {
				return nil, err
			}
			// also set time limit on reading
			c.SetDeadline(time.Now().Add(timeout))
			return c, nil
		}}
	proxy := &ProxyHandler{
		Transport: tr,
		HMacKey:   []byte(config.HmacKey)}

	// build/compile regex
	proxy.RegexpAllowlist = make([]*regexp.Regexp, 0)
	proxy.RegexpDenylist = make([]*regexp.Regexp, 0)

	var c *regexp.Regexp
	var err error
	for _, v := range config.Denylist {
		c, err = regexp.Compile(v)
		if err != nil {
			log.Fatal(err)
		}
		proxy.RegexpDenylist = append(proxy.RegexpDenylist, c)
	}
	for _, v := range config.Allowlist {
		c, err = regexp.Compile(v)
		if err != nil {
			log.Fatal(err)
		}
		proxy.RegexpAllowlist = append(proxy.RegexpAllowlist, c)
	}

	http.Handle("/favicon.ico", http.NotFoundHandler())
	http.Handle("/", proxy)
	log.Println("Starting server on", *bindAddress)
	err = http.ListenAndServe(*bindAddress, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err.Error())
	}
}
