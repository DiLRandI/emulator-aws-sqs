package tests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	sqssdk "github.com/aws/aws-sdk-go-v2/service/sqs"
	smithyendpoints "github.com/aws/smithy-go/endpoints"
)

const (
	testRegion = "us-east-1"
	testAccess = "test"
	testSecret = "test"
)

var (
	buildOnce sync.Once
	buildErr  error
	buildPath string
)

type integrationServer struct {
	baseURL string
	cmd     *exec.Cmd
	client  *http.Client
}

func startIntegrationServer(t *testing.T) *integrationServer {
	t.Helper()

	bin := buildBinary(t)
	port := reservePort(t)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	tempDir := t.TempDir()
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", filepath.Join(tempDir, "sqs.db"))

	cmd := exec.Command(bin)
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(),
		"SQS_LISTEN_ADDR="+fmt.Sprintf("127.0.0.1:%d", port),
		"SQS_PUBLIC_BASE_URL="+baseURL,
		"SQS_REGION="+testRegion,
		"SQS_ALLOWED_REGIONS="+testRegion,
		"SQS_SQLITE_DSN="+dsn,
		"SQS_AUTH_MODE=strict",
		"SQS_CREATE_PROPAGATION=0s",
		"SQS_ATTRIBUTE_PROPAGATION=0s",
		"SQS_RETENTION_PROPAGATION=0s",
		"SQS_DELETE_COOLDOWN=1s",
		"SQS_PURGE_COOLDOWN=1s",
		"SQS_LONG_POLL_WAKE_FREQUENCY=50ms",
	)
	output, err := os.Create(filepath.Join(tempDir, "sqsd.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		_ = output.Close()
	})

	client := &http.Client{Timeout: 10 * time.Second}
	waitForHTTPReady(t, client, baseURL)
	return &integrationServer{
		baseURL: baseURL,
		cmd:     cmd,
		client:  client,
	}
}

func buildBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "sqsd-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		buildPath = filepath.Join(tmpDir, "sqsd")
		cmd := exec.Command("go", "build", "-o", buildPath, "./cmd/sqsd")
		cmd.Dir = repoRoot(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build failed: %w: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildPath
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func reservePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitForHTTPReady(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodPost, baseURL+"/", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("X-Amz-Target", "AmazonSQS.ListQueues")
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusBadRequest {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("sqsd did not become ready")
}

func signedJSONRequest(t *testing.T, method, endpoint, target string, payload map[string]any) *http.Request {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonSQS."+target)
	signRequest(t, req, body)
	return req
}

func signedQueryRequest(t *testing.T, method, endpoint string, values url.Values) *http.Request {
	t.Helper()
	encoded := values.Encode()
	req, err := http.NewRequest(method, endpoint, strings.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, []byte(encoded))
	return req
}

func signRequest(t *testing.T, req *http.Request, body []byte) {
	t.Helper()
	signer := v4.NewSigner()
	hash := sha256.Sum256(body)
	creds := credentials.NewStaticCredentialsProvider(testAccess, testSecret, "")
	value, err := creds.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.SignHTTP(context.Background(), value, req, hex.EncodeToString(hash[:]), "sqs", testRegion, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func doJSON(t *testing.T, client *http.Client, req *http.Request, out any) *http.Response {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil && len(raw) != 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode json: %v body=%s", err, raw)
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp
}

func mustSDKConfig(t *testing.T) awsconfig.LoadOptionsFunc {
	t.Helper()
	return func(o *awsconfig.LoadOptions) error {
		o.Region = testRegion
		o.Credentials = credentials.NewStaticCredentialsProvider(testAccess, testSecret, "")
		return nil
	}
}

func newSDKClientBaseEndpoint(t *testing.T, endpoint string) *sqssdk.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), mustSDKConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseEndpoint = aws.String(endpoint)
	return sqssdk.NewFromConfig(cfg)
}

type resolverV2 struct {
	endpoint string
}

func (r resolverV2) ResolveEndpoint(_ context.Context, params sqssdk.EndpointParameters) (smithyendpoints.Endpoint, error) {
	uri, err := url.Parse(r.endpoint)
	if err != nil {
		return smithyendpoints.Endpoint{}, err
	}
	return smithyendpoints.Endpoint{URI: *uri}, nil
}

func newSDKClientResolverV2(t *testing.T, endpoint string) *sqssdk.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		mustSDKConfig(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	return sqssdk.NewFromConfig(cfg, func(o *sqssdk.Options) {
		o.EndpointResolverV2 = resolverV2{endpoint: endpoint}
	})
}
