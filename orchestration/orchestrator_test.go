package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"
	gows "github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/flags"

	"github.com/cloudflare/cloudflared/config"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/ingress"
	"github.com/cloudflare/cloudflared/management"
	"github.com/cloudflare/cloudflared/tracing"
	"github.com/cloudflare/cloudflared/tunnelrpc/pogs"
)

var (
	testLogger = zerolog.Nop()
	testTags   = []pogs.Tag{
		{
			Name:  "package",
			Value: "orchestration",
		},
		{
			Name:  "purpose",
			Value: "test",
		},
	}
	testDefaultDialer = ingress.NewDialer(ingress.WarpRoutingConfig{
		ConnectTimeout: config.CustomDuration{Duration: 1 * time.Second},
		TCPKeepAlive:   config.CustomDuration{Duration: 15 * time.Second},
		MaxActiveFlows: 0,
	})
)

// TestUpdateConfiguration tests that
// - configurations can be deserialized
// - proxy can be updated
// - last applied version and error are returned
// - configurations can be deserialized
// - receiving an old version is noop
func TestUpdateConfiguration(t *testing.T) {
	originDialer := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer:   testDefaultDialer,
		TCPWriteTimeout: 1 * time.Second,
	}, &testLogger)
	initConfig := &Config{
		Ingress:             &ingress.Ingress{},
		OriginDialerService: originDialer,
	}
	orchestrator, err := NewOrchestrator(t.Context(), initConfig, testTags, []ingress.Rule{ingress.NewManagementRule(management.New("management.argotunnel.com", false, "1.1.1.1:80", uuid.Nil, "", &testLogger, nil))}, &testLogger)
	require.NoError(t, err)
	initOriginProxy, err := orchestrator.GetOriginProxy()
	require.NoError(t, err)
	require.Implements(t, (*connection.OriginProxy)(nil), initOriginProxy)

	configJSONV2 := []byte(`
{
	"unknown_field": "not_deserialized",
    "originRequest": {
        "connectTimeout": 90,
		"noHappyEyeballs": true
    },
    "ingress": [
        {
            "hostname": "jira.tunnel.org",
			"path": "^\/login",
            "service": "http://192.16.19.1:443",
            "originRequest": {
                "noTLSVerify": true,
                "connectTimeout": 10
            }
        },
		{
            "hostname": "jira.tunnel.org",
            "service": "http://172.32.20.6:80",
            "originRequest": {
                "noTLSVerify": true,
                "connectTimeout": 30
            }
        },
        {
            "service": "http_status:404"
        }
    ],
    "warp-routing": {
        "connectTimeout": 10
    }
}
`)

	updateWithValidation(t, orchestrator, 2, configJSONV2)
	configV2 := orchestrator.config
	// Validate internal ingress rules
	require.Equal(t, "management.argotunnel.com", configV2.Ingress.InternalRules[0].Hostname)
	require.True(t, configV2.Ingress.InternalRules[0].Matches("management.argotunnel.com", "/ping"))
	require.Equal(t, "management", configV2.Ingress.InternalRules[0].Service.String())
	// Validate ingress rule 0
	require.Equal(t, "jira.tunnel.org", configV2.Ingress.Rules[0].Hostname)
	require.True(t, configV2.Ingress.Rules[0].Matches("jira.tunnel.org", "/login"))
	require.True(t, configV2.Ingress.Rules[0].Matches("jira.tunnel.org", "/login/2fa"))
	require.False(t, configV2.Ingress.Rules[0].Matches("jira.tunnel.org", "/users"))
	require.Equal(t, "http://192.16.19.1:443", configV2.Ingress.Rules[0].Service.String())
	require.Len(t, configV2.Ingress.Rules, 3)
	// originRequest of this ingress rule overrides global default
	require.Equal(t, config.CustomDuration{Duration: time.Second * 10}, configV2.Ingress.Rules[0].Config.ConnectTimeout)
	require.True(t, configV2.Ingress.Rules[0].Config.NoTLSVerify)
	// Inherited from global default
	require.True(t, configV2.Ingress.Rules[0].Config.NoHappyEyeballs)
	// Validate ingress rule 1
	require.Equal(t, "jira.tunnel.org", configV2.Ingress.Rules[1].Hostname)
	require.True(t, configV2.Ingress.Rules[1].Matches("jira.tunnel.org", "/users"))
	require.Equal(t, "http://172.32.20.6:80", configV2.Ingress.Rules[1].Service.String())
	// originRequest of this ingress rule overrides global default
	require.Equal(t, config.CustomDuration{Duration: time.Second * 30}, configV2.Ingress.Rules[1].Config.ConnectTimeout)
	require.True(t, configV2.Ingress.Rules[1].Config.NoTLSVerify)
	// Inherited from global default
	require.True(t, configV2.Ingress.Rules[1].Config.NoHappyEyeballs)
	// Validate ingress rule 2, it's the catch-all rule
	require.True(t, configV2.Ingress.Rules[2].Matches("blogs.tunnel.io", "/2022/02/10"))
	// Inherited from global default
	require.Equal(t, config.CustomDuration{Duration: time.Second * 90}, configV2.Ingress.Rules[2].Config.ConnectTimeout)
	require.False(t, configV2.Ingress.Rules[2].Config.NoTLSVerify)
	require.True(t, configV2.Ingress.Rules[2].Config.NoHappyEyeballs)
	require.Equal(t, 10*time.Second, configV2.WarpRouting.ConnectTimeout.Duration)

	originProxyV2, err := orchestrator.GetOriginProxy()
	require.NoError(t, err)
	require.Implements(t, (*connection.OriginProxy)(nil), originProxyV2)
	require.NotEqual(t, originProxyV2, initOriginProxy)

	// Should not downgrade to an older version
	resp := orchestrator.UpdateConfig(1, nil)
	require.NoError(t, resp.Err)
	require.Equal(t, int32(2), resp.LastAppliedVersion)

	invalidJSON := []byte(`
{
	"originRequest":
}

`)

	resp = orchestrator.UpdateConfig(3, invalidJSON)
	require.Error(t, resp.Err)
	require.Equal(t, int32(2), resp.LastAppliedVersion)
	originProxyV3, err := orchestrator.GetOriginProxy()
	require.NoError(t, err)
	require.Equal(t, originProxyV2, originProxyV3)

	configJSONV10 := []byte(`
{
    "ingress": [
        {
            "service": "hello-world"
        }
    ],
    "warp-routing": {
    }
}
`)
	updateWithValidation(t, orchestrator, 10, configJSONV10)
	configV10 := orchestrator.config
	require.Len(t, configV10.Ingress.Rules, 1)
	require.True(t, configV10.Ingress.Rules[0].Matches("blogs.tunnel.io", "/2022/02/10"))
	require.Equal(t, ingress.HelloWorldService, configV10.Ingress.Rules[0].Service.String())

	originProxyV10, err := orchestrator.GetOriginProxy()
	require.NoError(t, err)
	require.Implements(t, (*connection.OriginProxy)(nil), originProxyV10)
	require.NotEqual(t, originProxyV10, originProxyV2)
}

// Validates that a new version 0 will be applied if the configuration is loaded locally.
// This will happen when a locally managed tunnel is migrated to remote configuration and receives its first configuration.
func TestUpdateConfiguration_FromMigration(t *testing.T) {
	originDialer := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer:   testDefaultDialer,
		TCPWriteTimeout: 1 * time.Second,
	}, &testLogger)
	initConfig := &Config{
		Ingress:             &ingress.Ingress{},
		OriginDialerService: originDialer,
	}
	orchestrator, err := NewOrchestrator(t.Context(), initConfig, testTags, []ingress.Rule{}, &testLogger)
	require.NoError(t, err)
	initOriginProxy, err := orchestrator.GetOriginProxy()
	require.NoError(t, err)
	require.Implements(t, (*connection.OriginProxy)(nil), initOriginProxy)

	configJSONV2 := []byte(`
{
    "ingress": [
        {
            "service": "http_status:404"
        }
    ],
    "warp-routing": {
    }
}
`)
	updateWithValidation(t, orchestrator, 0, configJSONV2)
	require.Len(t, orchestrator.config.Ingress.Rules, 1)
}

// Validates that the default ingress rule will be set if there is no rule provided from the remote.
func TestUpdateConfiguration_WithoutIngressRule(t *testing.T) {
	originDialer := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer:   testDefaultDialer,
		TCPWriteTimeout: 1 * time.Second,
	}, &testLogger)
	initConfig := &Config{
		Ingress:             &ingress.Ingress{},
		OriginDialerService: originDialer,
	}
	orchestrator, err := NewOrchestrator(t.Context(), initConfig, testTags, []ingress.Rule{}, &testLogger)
	require.NoError(t, err)
	initOriginProxy, err := orchestrator.GetOriginProxy()
	require.NoError(t, err)
	require.Implements(t, (*connection.OriginProxy)(nil), initOriginProxy)

	// We need to create an empty RemoteConfigJSON because that will get unmarshalled to a RemoteConfig
	emptyConfig := &ingress.RemoteConfigJSON{}
	configBytes, err := json.Marshal(emptyConfig)
	if err != nil {
		require.FailNow(t, "The RemoteConfigJSON shouldn't fail while being marshalled")
	}

	updateWithValidation(t, orchestrator, 0, configBytes)
	require.Len(t, orchestrator.config.Ingress.Rules, 1)
}

// TestConcurrentUpdateAndRead makes sure orchestrator can receive updates and return origin proxy concurrently
func TestConcurrentUpdateAndRead(t *testing.T) {
	const (
		concurrentRequests = 200
		hostname           = "public.tunnels.org"
		expectedHost       = "internal.tunnels.svc.cluster.local"
		tcpBody            = "testProxyTCP"
	)

	httpOrigin := httptest.NewServer(&validateHostHandler{
		expectedHost: expectedHost,
		body:         t.Name(),
	})
	defer httpOrigin.Close()

	tcpOrigin, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer tcpOrigin.Close()

	originDialer := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer:   testDefaultDialer,
		TCPWriteTimeout: 1 * time.Second,
	}, &testLogger)

	var (
		configJSONV1 = []byte(fmt.Sprintf(`
{
    "originRequest": {
        "connectTimeout": 90,
		"noHappyEyeballs": true
    },
    "ingress": [
        {
            "hostname": "%s",
            "service": "%s",
            "originRequest": {
				"httpHostHeader": "%s",
                "connectTimeout": 10
            }
        },
        {
            "service": "http_status:404"
        }
    ],
    "warp-routing": {
    }
}
`, hostname, httpOrigin.URL, expectedHost))
		configJSONV2 = []byte(`
{
    "ingress": [
        {
            "service": "http_status:204"
        }
    ],
    "warp-routing": {
    }
}
`)

		configJSONV3 = []byte(`
{
    "ingress": [
        {
            "service": "http_status:418"
        }
    ],
    "warp-routing": {
    }
}
`)

		// appliedV2 makes sure v3 is applied after v2
		appliedV2 = make(chan struct{})

		initConfig = &Config{
			Ingress:             &ingress.Ingress{},
			OriginDialerService: originDialer,
		}
	)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	orchestrator, err := NewOrchestrator(ctx, initConfig, testTags, []ingress.Rule{}, &testLogger)
	require.NoError(t, err)

	updateWithValidation(t, orchestrator, 1, configJSONV1)

	var wg sync.WaitGroup
	// tcpOrigin will be closed when the test exits. Only the handler routines are included in the wait group
	go func() {
		serveTCPOrigin(t, tcpOrigin, &wg)
	}()
	for i := range concurrentRequests {
		originProxy, err := orchestrator.GetOriginProxy()
		require.NoError(t, err)
		wg.Add(1)
		go func(i int, originProxy connection.OriginProxy) {
			defer wg.Done()
			resp, err := proxyHTTP(originProxy, hostname)
			assert.NoError(t, err, "proxyHTTP %d failed %v", i, err)
			defer resp.Body.Close()

			// The response can be from initOrigin, http_status:204 or http_status:418
			switch resp.StatusCode {
			// v1 proxy
			case 200:
				body, err := io.ReadAll(resp.Body)
				assert.NoError(t, err)
				assert.Equal(t, t.Name(), string(body))
			// v2 proxy
			case 204:
				assert.Greater(t, i, concurrentRequests/4)
			// v3 proxy
			case 418:
				assert.Greater(t, i, concurrentRequests/2)
			}

			// Once we have originProxy, it won't be changed by configuration updates.
			// We can infer the version by the ProxyHTTP response code
			pr, pw := io.Pipe()
			w := newRespReadWriteFlusher()

			// Write TCP message and make sure it's echo back. This has to be done in a go routune since ProxyTCP doesn't
			// return until the stream is closed.
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer pw.Close()
				tcpEyeball(t, pw, tcpBody, w)
			}()

			err = proxyTCP(ctx, originProxy, tcpOrigin.Addr().String(), w, pr)
			assert.NoError(t, err, "proxyTCP %d failed %v", i, err)
		}(i, originProxy)

		if i == concurrentRequests/4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				updateWithValidation(t, orchestrator, 2, configJSONV2)
				close(appliedV2)
			}()
		}

		if i == concurrentRequests/2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Makes sure v2 is applied before v3
				<-appliedV2
				updateWithValidation(t, orchestrator, 3, configJSONV3)
			}()
		}
	}

	wg.Wait()
}

// TestOverrideWarpRoutingConfigWithLocalValues tests that if a value is defined in the Config.ConfigurationFlags,
// it will override the value that comes from the remote result.
func TestOverrideWarpRoutingConfigWithLocalValues(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	assertMaxActiveFlows := func(orchestrator *Orchestrator, expectedValue uint64) {
		configJson, err := orchestrator.GetConfigJSON()
		require.NoError(t, err)
		var result map[string]interface{}
		err = json.Unmarshal(configJson, &result)
		require.NoError(t, err)
		warpRouting := result["warp-routing"].(map[string]interface{})
		require.EqualValues(t, expectedValue, warpRouting["maxActiveFlows"])
	}

	originDialer := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer:   testDefaultDialer,
		TCPWriteTimeout: 1 * time.Second,
	}, &testLogger)

	// All the possible values set for MaxActiveFlows from the various points that can provide it:
	// 1. Initialized value
	// 2. Local CLI flag config
	// 3. Remote configuration value
	initValue := uint64(0)
	localValue := uint64(100)
	remoteValue := uint64(500)

	initConfig := &Config{
		Ingress: &ingress.Ingress{},
		WarpRouting: ingress.WarpRoutingConfig{
			MaxActiveFlows: initValue,
		},
		OriginDialerService: originDialer,
		ConfigurationFlags: map[string]string{
			flags.MaxActiveFlows: fmt.Sprintf("%d", localValue),
		},
	}

	// We expect the local configuration flag to be the starting value
	orchestrator, err := NewOrchestrator(ctx, initConfig, testTags, []ingress.Rule{}, &testLogger)
	require.NoError(t, err)

	assertMaxActiveFlows(orchestrator, localValue)

	// Assigning the MaxActiveFlows in the remote config should be ignored over the local config
	remoteWarpConfig := ingress.WarpRoutingConfig{
		MaxActiveFlows: remoteValue,
	}

	// Force a configuration refresh
	err = orchestrator.updateIngress(ingress.Ingress{}, remoteWarpConfig)
	require.NoError(t, err)

	// Check the value being used is the local one
	assertMaxActiveFlows(orchestrator, localValue)
}

func proxyHTTP(originProxy connection.OriginProxy, hostname string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s", hostname), nil)
	if err != nil {
		return nil, err
	}

	w := httptest.NewRecorder()
	log := zerolog.Nop()
	respWriter, err := connection.NewHTTP2RespWriter(req, w, connection.TypeHTTP, &log)
	if err != nil {
		return nil, err
	}

	err = originProxy.ProxyHTTP(respWriter, tracing.NewTracedHTTPRequest(req, 0, &log), false)
	if err != nil {
		return nil, err
	}

	return w.Result(), nil
}

// nolint: testifylint // this is used inside go routines so it can't use `require.`
func tcpEyeball(t *testing.T, reqWriter io.WriteCloser, body string, respReadWriter *respReadWriteFlusher) {
	writeN, err := reqWriter.Write([]byte(body))
	assert.NoError(t, err)

	readBuffer := make([]byte, writeN)
	n, err := respReadWriter.Read(readBuffer)
	assert.NoError(t, err)
	assert.Equal(t, body, string(readBuffer[:n]))
	assert.Equal(t, writeN, n)
}

func proxyTCP(ctx context.Context, originProxy connection.OriginProxy, originAddr string, w http.ResponseWriter, reqBody io.ReadCloser) error {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s", originAddr), reqBody)
	if err != nil {
		return err
	}

	log := zerolog.Nop()
	respWriter, err := connection.NewHTTP2RespWriter(req, w, connection.TypeTCP, &log)
	if err != nil {
		return err
	}

	tcpReq := &connection.TCPRequest{
		Dest:    originAddr,
		CFRay:   "123",
		LBProbe: false,
	}
	rws := connection.NewHTTPResponseReadWriterAcker(respWriter, w.(http.Flusher), req)

	return originProxy.ProxyTCP(ctx, rws, tcpReq)
}

func serveTCPOrigin(t *testing.T, tcpOrigin net.Listener, wg *sync.WaitGroup) {
	for {
		conn, err := tcpOrigin.Accept()
		if err != nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()

			echoTCP(t, conn)
		}()
	}
}

// nolint: testifylint // this is used inside go routines so it can't use `require.`
func echoTCP(t *testing.T, conn net.Conn) {
	readBuf := make([]byte, 1000)
	readN, err := conn.Read(readBuf)
	assert.NoError(t, err)

	writeN, err := conn.Write(readBuf[:readN])
	assert.NoError(t, err)
	assert.Equal(t, readN, writeN)
}

type validateHostHandler struct {
	expectedHost string
	body         string
}

func (vhh *validateHostHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Host != vhh.expectedHost {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(vhh.body))
}

// nolint: testifylint // this is used inside go routines so it can't use `require.`
func updateWithValidation(t *testing.T, orchestrator *Orchestrator, version int32, config []byte) {
	resp := orchestrator.UpdateConfig(version, config)
	assert.NoError(t, resp.Err)
	assert.Equal(t, version, resp.LastAppliedVersion)
}

// TestClosePreviousProxies makes sure proxies started in the previous configuration version are shutdown
func TestClosePreviousProxies(t *testing.T) {
	originDialer := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer:   testDefaultDialer,
		TCPWriteTimeout: 1 * time.Second,
	}, &testLogger)
	var (
		hostname             = "hello.tunnel1.org"
		configWithHelloWorld = []byte(fmt.Sprintf(`
{
    "ingress": [
        {
			"hostname": "%s",
            "service": "hello-world"
        },
		{
			"service": "http_status:404"
		}
    ],
    "warp-routing": {
    }
}
`, hostname))

		configTeapot = []byte(`
{
    "ingress": [
		{
			"service": "http_status:418"
		}
    ],
    "warp-routing": {
    }
}
`)
		initConfig = &Config{
			Ingress:             &ingress.Ingress{},
			OriginDialerService: originDialer,
		}
	)

	ctx, cancel := context.WithCancel(t.Context())
	orchestrator, err := NewOrchestrator(ctx, initConfig, testTags, []ingress.Rule{}, &testLogger)
	require.NoError(t, err)

	updateWithValidation(t, orchestrator, 1, configWithHelloWorld)

	originProxyV1, err := orchestrator.GetOriginProxy()
	require.NoError(t, err)
	// nolint: bodyclose
	resp, err := proxyHTTP(originProxyV1, hostname)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	updateWithValidation(t, orchestrator, 2, configTeapot)

	originProxyV2, err := orchestrator.GetOriginProxy()
	require.NoError(t, err)
	// nolint: bodyclose
	resp, err = proxyHTTP(originProxyV2, hostname)
	require.NoError(t, err)
	require.Equal(t, http.StatusTeapot, resp.StatusCode)

	// The hello-world server in config v1 should have been stopped. We wait a bit since it's closed asynchronously.
	time.Sleep(time.Millisecond * 10)
	// nolint: bodyclose
	resp, err = proxyHTTP(originProxyV1, hostname)
	require.Error(t, err)
	require.Nil(t, resp)

	// Apply the config with hello world server again, orchestrator should spin up another hello world server
	updateWithValidation(t, orchestrator, 3, configWithHelloWorld)

	originProxyV3, err := orchestrator.GetOriginProxy()
	require.NoError(t, err)
	require.NotEqual(t, originProxyV1, originProxyV3)

	// nolint: bodyclose
	resp, err = proxyHTTP(originProxyV3, hostname)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// cancel the context should terminate the last proxy
	cancel()
	// Wait for proxies to shutdown
	time.Sleep(time.Millisecond * 10)

	// nolint: bodyclose
	resp, err = proxyHTTP(originProxyV3, hostname)
	require.Error(t, err)
	require.Nil(t, resp)
}

// TestPersistentConnection makes sure updating the ingress doesn't intefere with existing connections
func TestPersistentConnection(t *testing.T) {
	const (
		hostname = "http://ws.tunnel.org"
	)
	msg := t.Name()
	originDialer := ingress.NewOriginDialer(ingress.OriginConfig{
		DefaultDialer:   testDefaultDialer,
		TCPWriteTimeout: 1 * time.Second,
	}, &testLogger)
	initConfig := &Config{
		Ingress:             &ingress.Ingress{},
		OriginDialerService: originDialer,
	}
	orchestrator, err := NewOrchestrator(t.Context(), initConfig, testTags, []ingress.Rule{}, &testLogger)
	require.NoError(t, err)

	wsOrigin := httptest.NewServer(http.HandlerFunc(wsEcho))
	defer wsOrigin.Close()

	tcpOrigin, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer tcpOrigin.Close()

	configWithWSAndWarp := []byte(fmt.Sprintf(`
{
    "ingress": [
        {
            "service": "%s"
        }
    ],
    "warp-routing": {
    }
}
`, wsOrigin.URL))

	updateWithValidation(t, orchestrator, 1, configWithWSAndWarp)

	originProxy, err := orchestrator.GetOriginProxy()
	require.NoError(t, err)

	wsReqReader, wsReqWriter := io.Pipe()
	wsRespReadWriter := newRespReadWriteFlusher()

	tcpReqReader, tcpReqWriter := io.Pipe()
	tcpRespReadWriter := newRespReadWriteFlusher()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(3)
	// Start TCP origin
	go func() {
		defer wg.Done()
		conn, err := tcpOrigin.Accept()
		assert.NoError(t, err)
		defer conn.Close()

		// Expect 3 TCP messages
		for i := 0; i < 3; i++ {
			echoTCP(t, conn)
		}
	}()
	// Simulate cloudflared receiving a TCP connection
	go func() {
		defer wg.Done()
		assert.NoError(t, proxyTCP(ctx, originProxy, tcpOrigin.Addr().String(), tcpRespReadWriter, tcpReqReader))
	}()
	// Simulate cloudflared receiving a WS connection
	go func() {
		defer wg.Done()

		req, err := http.NewRequest(http.MethodGet, hostname, wsReqReader)
		assert.NoError(t, err)
		// ProxyHTTP will add Connection, Upgrade and Sec-Websocket-Version headers
		req.Header.Add("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

		log := zerolog.Nop()
		respWriter, err := connection.NewHTTP2RespWriter(req, wsRespReadWriter, connection.TypeWebsocket, &log)
		assert.NoError(t, err)

		err = originProxy.ProxyHTTP(respWriter, tracing.NewTracedHTTPRequest(req, 0, &log), true)
		assert.NoError(t, err)
	}()

	// Simulate eyeball WS and TCP connections
	validateWsEcho(t, msg, wsReqWriter, wsRespReadWriter)
	tcpEyeball(t, tcpReqWriter, msg, tcpRespReadWriter)

	configNoWSAndWarp := []byte(`
{
    "ingress": [
        {
            "service": "http_status:404"
        }
    ],
    "warp-routing": {
    }
}
`)

	updateWithValidation(t, orchestrator, 2, configNoWSAndWarp)
	// Make sure connection is still up
	validateWsEcho(t, msg, wsReqWriter, wsRespReadWriter)
	tcpEyeball(t, tcpReqWriter, msg, tcpRespReadWriter)

	updateWithValidation(t, orchestrator, 3, configWithWSAndWarp)
	// Make sure connection is still up
	validateWsEcho(t, msg, wsReqWriter, wsRespReadWriter)
	tcpEyeball(t, tcpReqWriter, msg, tcpRespReadWriter)

	wsReqWriter.Close()
	tcpReqWriter.Close()
	wg.Wait()
}

func TestSerializeLocalConfig(t *testing.T) {
	c := &newLocalConfig{
		RemoteConfig: ingress.RemoteConfig{
			Ingress: ingress.Ingress{},
		},
		ConfigurationFlags: map[string]string{"a": "b"},
	}

	result, err := json.Marshal(c)
	require.NoError(t, err)
	require.JSONEq(t, `{"__configuration_flags":{"a":"b"},"ingress":[],"warp-routing":{"connectTimeout":0,"tcpKeepAlive":0}}`, string(result))
}

func wsEcho(w http.ResponseWriter, r *http.Request) {
	upgrader := gows.Upgrader{}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		mt, message, err := conn.ReadMessage()
		if err != nil {
			fmt.Println("read message err", err)
			break
		}
		err = conn.WriteMessage(mt, message)
		if err != nil {
			fmt.Println("write message err", err)
			break
		}
	}
}

func validateWsEcho(t *testing.T, msg string, reqWriter io.Writer, respReadWriter io.ReadWriter) {
	err := wsutil.WriteClientText(reqWriter, []byte(msg))
	require.NoError(t, err)

	receivedMsg, err := wsutil.ReadServerText(respReadWriter)
	require.NoError(t, err)
	require.Equal(t, msg, string(receivedMsg))
}

type respReadWriteFlusher struct {
	io.Reader
	w             io.Writer
	headers       http.Header
	statusCode    int
	setStatusOnce sync.Once
	hasStatus     chan struct{}
}

func newRespReadWriteFlusher() *respReadWriteFlusher {
	pr, pw := io.Pipe()
	return &respReadWriteFlusher{
		Reader:    pr,
		w:         pw,
		headers:   make(http.Header),
		hasStatus: make(chan struct{}),
	}
}

func (rrw *respReadWriteFlusher) Write(buf []byte) (int, error) {
	rrw.WriteHeader(http.StatusOK)
	return rrw.w.Write(buf)
}

func (rrw *respReadWriteFlusher) Flush() {}

func (rrw *respReadWriteFlusher) Header() http.Header {
	return rrw.headers
}

func (rrw *respReadWriteFlusher) WriteHeader(statusCode int) {
	rrw.setStatusOnce.Do(func() {
		rrw.statusCode = statusCode
		close(rrw.hasStatus)
	})
}
