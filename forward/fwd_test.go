package forward

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"crypto/tls"
	"crypto/x509"
	"io/ioutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vulcand/oxy/testutils"
	"github.com/vulcand/oxy/utils"
	"github.com/containous/traefik/log"
	"regexp"
)

const (
	certDirectory = "../testutils/certs/"
)

// Makes sure hop-by-hop headers are removed
func TestForwardHopHeaders(t *testing.T) {
	called := false
	var outHeaders http.Header
	var outHost, expectedHost string
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		called = true
		outHeaders = req.Header
		outHost = req.Host
		w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New()
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		expectedHost = req.URL.Host
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	headers := http.Header{
		Connection: []string{"close"},
		KeepAlive:  []string{"timeout=600"},
	}

	re, body, err := testutils.Get(proxy.URL, testutils.Headers(headers))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(body))
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, true, called)
	assert.Equal(t, "", outHeaders.Get(Connection))
	assert.Equal(t, "", outHeaders.Get(KeepAlive))
	assert.Equal(t, expectedHost, outHost)
}

func TestDefaultErrHandler(t *testing.T) {
	f, err := New()
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI("http://localhost:63450")
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, _, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadGateway, re.StatusCode)
}

func TestCustomErrHandler(t *testing.T) {
	f, err := New(ErrorHandler(utils.ErrorHandlerFunc(func(w http.ResponseWriter, req *http.Request, err error) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte(http.StatusText(http.StatusTeapot)))
	})))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI("http://localhost:63450")
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, body, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusTeapot, re.StatusCode)
	assert.Equal(t, http.StatusText(http.StatusTeapot), string(body))
}

func TestResponseModifier(t *testing.T) {
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New(ResponseModifier(func(resp *http.Response) error {
		resp.Header.Add("X-Test", "CUSTOM")
		return nil
	}))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, _, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, "CUSTOM", re.Header.Get("X-Test"))
}

// Makes sure hop-by-hop headers are removed
func TestForwardedHeaders(t *testing.T) {
	var outHeaders http.Header
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		outHeaders = req.Header
		w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New(Rewriter(&HeaderRewriter{TrustForwardHeader: true, Hostname: "hello"}))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	headers := http.Header{
		XForwardedProto:  []string{"httpx"},
		XForwardedFor:    []string{"192.168.1.1"},
		XForwardedServer: []string{"foobar"},
		XForwardedHost:   []string{"upstream-foobar"},
	}

	re, _, err := testutils.Get(proxy.URL, testutils.Headers(headers))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, "httpx", outHeaders.Get(XForwardedProto))
	assert.Contains(t, outHeaders.Get(XForwardedFor), "192.168.1.1")
	assert.Contains(t, "upstream-foobar", outHeaders.Get(XForwardedHost))
	assert.Equal(t, "hello", outHeaders.Get(XForwardedServer))
}

func TestCustomRewriter(t *testing.T) {
	var outHeaders http.Header
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		outHeaders = req.Header
		w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New(Rewriter(&HeaderRewriter{TrustForwardHeader: false, Hostname: "hello"}))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	headers := http.Header{
		XForwardedProto: []string{"httpx"},
		XForwardedFor:   []string{"192.168.1.1"},
	}

	re, _, err := testutils.Get(proxy.URL, testutils.Headers(headers))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, "http", outHeaders.Get(XForwardedProto))
	assert.NotContains(t, outHeaders.Get(XForwardedFor), "192.168.1.1")
}

func TestCustomTransportTimeout(t *testing.T) {
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		time.Sleep(20 * time.Millisecond)
		w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New(RoundTripper(
		&http.Transport{
			ResponseHeaderTimeout: 5 * time.Millisecond,
		}))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, _, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusGatewayTimeout, re.StatusCode)
}

func TestCustomLogger(t *testing.T) {
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New()
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, _, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
}

func TestRouteForwarding(t *testing.T) {
	var outPath string
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		outPath = req.RequestURI
		w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New()
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	tests := []struct {
		Path  string
		Query string

		ExpectedPath string
	}{
		{"/hello", "", "/hello"},
		{"//hello", "", "//hello"},
		{"///hello", "", "///hello"},
		{"/hello", "abc=def&def=123", "/hello?abc=def&def=123"},
		{"/log/http%3A%2F%2Fwww.site.com%2Fsomething?a=b", "", "/log/http%3A%2F%2Fwww.site.com%2Fsomething?a=b"},
	}

	for _, test := range tests {
		proxyURL := proxy.URL + test.Path
		if test.Query != "" {
			proxyURL = proxyURL + "?" + test.Query
		}
		request, err := http.NewRequest("GET", proxyURL, nil)
		require.NoError(t, err)

		re, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, re.StatusCode)
		assert.Equal(t, test.ExpectedPath, outPath)
	}
}

func TestForwardedProto(t *testing.T) {
	var proto string
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		proto = req.Header.Get(XForwardedProto)
		w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New()
	require.NoError(t, err)

	proxy := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	tproxy := httptest.NewUnstartedServer(proxy)
	tproxy.StartTLS()
	defer tproxy.Close()

	re, _, err := testutils.Get(tproxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, "https", proto)
}

func TestChunkedResponseConversion(t *testing.T) {
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		h := w.(http.Hijacker)
		conn, _, _ := h.Hijack()
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4\r\ntest\r\n5\r\ntest1\r\n5\r\ntest2\r\n0\r\n\r\n")
		conn.Close()
	})
	defer srv.Close()

	f, err := New()
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	defer proxy.Close()

	re, body, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, "testtest1test2", string(body))
	assert.Equal(t, http.StatusOK, re.StatusCode)
	assert.Equal(t, fmt.Sprintf("%d", len("testtest1test2")), re.Header.Get("Content-Length"))
}

func TestContextWithValueInErrHandler(t *testing.T) {
	var originalPBool *bool
	originalBool := false
	originalPBool = &originalBool

	f, err := New(ErrorHandler(utils.ErrorHandlerFunc(func(rw http.ResponseWriter, req *http.Request, err error) {
		test, isBool := req.Context().Value("test").(*bool)
		if isBool {
			*test = true
		}
		if err != nil {
			rw.WriteHeader(http.StatusBadGateway)
		}
	})))
	require.NoError(t, err)

	proxy := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		// We need a network error
		req.URL = testutils.ParseURI("http://localhost:63450")
		newReq := req.WithContext(context.WithValue(req.Context(), "test", originalPBool))
		f.ServeHTTP(w, newReq)
	})
	defer proxy.Close()

	re, _, err := testutils.Get(proxy.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadGateway, re.StatusCode)
	assert.True(t, *originalPBool)
}

func getExpectedCert(t *testing.T, certName string) string{
	pem, err := ioutil.ReadFile(certDirectory + certName + ".crt")
	if err != nil {
		t.Error(err)
	}

	var re = regexp.MustCompile("-----BEGIN CERTIFICATE-----(?s)(.*)")
	cert := re.FindString(string(pem))
	return sanitize([]byte(cert))
}

func TestForwardClientTLSCert(t *testing.T) {
	tests := []struct {
		certNames  []string

		ExpectedHeaderValue string
	}{
		{[]string{"minimal"}, getExpectedCert(t, "minimal")},
		{[]string{"simple"}, getExpectedCert(t, "simple")},
		{[]string{"cheese"}, getExpectedCert(t,"cheese")},
	}

	var outHeaders http.Header
	srv := testutils.NewHandler(func(w http.ResponseWriter, req *http.Request) {
		outHeaders = req.Header
		w.Write([]byte("hello"))
	})
	defer srv.Close()

	f, err := New(PassClientCert(true))

	require.Nil(t, err)

	proxy := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.URL = testutils.ParseURI(srv.URL)
		f.ServeHTTP(w, req)
	})
	tproxy := httptest.NewUnstartedServer(proxy)
	clientCACert, err := ioutil.ReadFile(certDirectory+"ca.crt")
	if err != nil {
		require.Nil(t, err)
	}
	clientCertPool := x509.NewCertPool()
	clientCertPool.AppendCertsFromPEM(clientCACert)
	tproxy.TLS = &tls.Config{
		InsecureSkipVerify: true,
		ClientAuth:         tls.RequireAndVerifyClientCert,
		ClientCAs:          clientCertPool,
	}
	tproxy.StartTLS()
	defer tproxy.Close()

	for _, test := range tests {
		pem, err := ioutil.ReadFile(certDirectory + test.certNames[0] + ".crt")
		if err != nil {
			t.Error(err)
		}
		log.Printf("pem: %s", pem)
		re, _, err := testutils.Get(tproxy.URL, testutils.PassClientCert(test.certNames))
		require.Nil(t, err)
		require.Equal(t, http.StatusOK, re.StatusCode)
		require.Equal(t, "https", outHeaders.Get(XForwardedProto))
		require.Equal(t, test.ExpectedHeaderValue, outHeaders.Get(XForwardedSSLClientCert))
	}

}