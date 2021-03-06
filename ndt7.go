// Package ndt7 contains a ndt7 client.
//
// The client will automatically discover a suitable server to use
// by default. However, you can also manually discover a server and
// configure the client accordingly.
//
// The code configures reasonable I/O timeouts. We recommend to also
// provide contexts with whole-operation timeouts attached, as we
// do in the code example provided as part of this package.
package ndt7

import (
	"context"
	"net/http"
	"net/url"
	"runtime"
	"time"

	"github.com/gorilla/websocket"
	"github.com/m-lab/ndt7-client-go/internal/download"
	"github.com/m-lab/ndt7-client-go/internal/params"
	"github.com/m-lab/ndt7-client-go/internal/upload"
	"github.com/m-lab/ndt7-client-go/internal/websocketx"
	"github.com/m-lab/ndt7-client-go/mlabns"
	"github.com/m-lab/ndt7-client-go/spec"
)

const (
	// libraryName is the name of this library
	libraryName = "ndt7-client-go"

	// libraryVersion is the version of this library
	libraryVersion = "0.1.0"
)

// locateFn is the type of function used to locate a server.
type locateFn = func(ctx context.Context, client *mlabns.Client) (string, error)

// connectFn is the type of the function used to create
// a new *websocket.Conn connection.
type connectFn = func(
	dialer websocket.Dialer,
	ctx context.Context, urlStr string,
	requestHeader http.Header,
) (*websocket.Conn, *http.Response, error)

// testFn is the type of the function running a test.
type testFn = func(
	ctx context.Context, conn websocketx.Conn, ch chan<- spec.Measurement,
) error

// DefaultWebSocketHandshakeTimeout is the default timeout configured
// by NewClient in the Client.Dialer.HandshakeTimeout field.
const DefaultWebSocketHandshakeTimeout = 7 * time.Second

// LatestMeasurements contains the latest Measurement sent by the server and the client,
// plus the latest ConnectionInfo sent by the server.
type LatestMeasurements struct {
	Server         spec.Measurement
	Client         spec.Measurement
	ConnectionInfo *spec.ConnectionInfo
}

// Client is a ndt7 client.
type Client struct {
	// ClientName is the name of the software running ndt7 tests. It's set by
	// NewClient; you may want to change this value.
	ClientName string

	// ClientVersion is the version of the software running ndt7 tests. It's
	// set by NewClient; you may want to change this value.
	ClientVersion string

	// Dialer is the optional websocket Dialer. It's set to its
	// default value by NewClient; you may override it.
	Dialer websocket.Dialer

	// FQDN is the optional server FQDN. We will discover the FQDN of
	// a nearby M-Lab server for you if this field is empty.
	FQDN string

	// MLabNSClient is the mlabns client. We'll configure it with
	// defaults in NewClient and you may override it.
	MLabNSClient *mlabns.Client

	// Scheme is the scheme to use. It's set to "wss" by NewClient,
	// change it to "ws" for unencrypted ndt7.
	Scheme string

	// connect is the function for connecting a specific
	// websocket cnnection. It's set to its default value by
	// NewClient, but you may override it.
	connect connectFn

	// download is the function running the download test. We
	// set it in NewClient and you may override it.
	download testFn

	// locate is the optional function to locate a ndt7 server using
	// the mlab-ns service. This function is set to its default value
	// by NewClient, but you may want to override it.
	locate locateFn

	// upload is like download but for the upload test.
	upload testFn

	results map[spec.TestKind]*LatestMeasurements
}

// makeUserAgent creates the user agent string
func makeUserAgent(clientName, clientVersion string) string {
	return clientName + "/" + clientVersion + " " + libraryName + "/" + libraryVersion
}

// NewClient creates a new client instance identified by the specified
// clientName and clientVersion. M-Lab services may reject requests coming
// from clients that do not identify themselves properly.
func NewClient(clientName, clientVersion string) *Client {
	results := map[spec.TestKind]*LatestMeasurements{
		spec.TestDownload: &LatestMeasurements{},
		spec.TestUpload:   &LatestMeasurements{},
	}
	return &Client{
		ClientName:    clientName,
		ClientVersion: clientVersion,
		connect: func(
			dialer websocket.Dialer, ctx context.Context, urlStr string,
			requestHeader http.Header) (*websocket.Conn, *http.Response, error,
		) {
			return dialer.DialContext(ctx, urlStr, requestHeader)
		},
		Dialer: websocket.Dialer{
			HandshakeTimeout: DefaultWebSocketHandshakeTimeout,
		},
		download: download.Run,
		locate: func(ctx context.Context, c *mlabns.Client) (string, error) {
			return c.Query(ctx)
		},
		MLabNSClient: mlabns.NewClient(
			"ndt7", makeUserAgent(clientName, clientVersion),
		),
		upload:  upload.Run,
		Scheme:  "wss",
		results: results,
	}
}

// discoverServer discovers and returns the closest mlab server.
func (c *Client) discoverServer(ctx context.Context) (string, error) {
	return c.locate(ctx, c.MLabNSClient)
}

// doConnect establishes a websocket connection.
func (c *Client) doConnect(ctx context.Context, URLPath string) (*websocket.Conn, error) {
	URL := url.URL{}
	URL.Scheme = c.Scheme
	URL.Host = c.FQDN
	URL.Path = URLPath
	q := URL.Query()
	q.Set("client_arch", runtime.GOARCH)
	q.Set("client_library_name", libraryName)
	q.Set("client_library_version", libraryVersion)
	q.Set("client_name", c.ClientName)
	q.Set("client_os", runtime.GOOS)
	q.Set("client_version", c.ClientVersion)
	URL.RawQuery = q.Encode()
	headers := http.Header{}
	headers.Add("Sec-WebSocket-Protocol", params.SecWebSocketProtocol)
	headers.Add("User-Agent", makeUserAgent(c.ClientName, c.ClientVersion))
	conn, _, err := c.connect(c.Dialer, ctx, URL.String(), headers)
	return conn, err
}

// start is the function for starting a test.
func (c *Client) start(ctx context.Context, f testFn, p string) (<-chan spec.Measurement, error) {
	if c.FQDN == "" {
		fqdn, err := c.discoverServer(ctx)
		if err != nil {
			return nil, err
		}
		c.FQDN = fqdn
	}
	conn, err := c.doConnect(ctx, p)
	if err != nil {
		return nil, err
	}
	ch := make(chan spec.Measurement)
	go c.collectData(ctx, f, conn, ch)
	return ch, nil
}

func (c *Client) collectData(ctx context.Context, f testFn, conn websocketx.Conn, outch chan<- spec.Measurement) {
	inch := make(chan spec.Measurement)
	defer close(outch)
	go f(ctx, conn, inch)

	for m := range inch {
		switch m.Origin {
		case spec.OriginClient:
			c.results[m.Test].Client = m
		case spec.OriginServer:
			// The server only sends ConnectionInfo once at the beginning of
			// the test, thus if we want to know the client IP and test UUID
			// we need to store it separately.
			if m.ConnectionInfo != nil {
				c.results[m.Test].ConnectionInfo = m.ConnectionInfo
			}
			c.results[m.Test].Server = m
		}
		outch <- m
	}
}

// StartDownload discovers a ndt7 server (if needed) and starts a download. On
// success it returns a channel where measurements are emitted. This channel is
// closed when the download ends. On failure, the error is non nil and you
// should not attempt using the channel. A side effect of starting the download
// is that, if you did not specify a server FQDN, we will discover a server
// for you and store that value into the c.FQDN field.
func (c *Client) StartDownload(ctx context.Context) (<-chan spec.Measurement, error) {
	return c.start(ctx, c.download, params.DownloadURLPath)
}

// StartUpload is like StartDownload but for the upload.
func (c *Client) StartUpload(ctx context.Context) (<-chan spec.Measurement, error) {
	return c.start(ctx, c.upload, params.UploadURLPath)
}

// Results returns the test results map.
func (c *Client) Results() map[spec.TestKind]*LatestMeasurements {
	return c.results
}
