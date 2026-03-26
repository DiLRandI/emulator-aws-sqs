package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"emulator-aws-sqs/internal/clock"
	apierrors "emulator-aws-sqs/internal/errors"
	"emulator-aws-sqs/internal/protocol"
)

type Credential struct {
	AccessKeyID     string     `json:"accessKeyId"`
	SecretAccessKey string     `json:"secretAccessKey"`
	SessionToken    string     `json:"sessionToken,omitempty"`
	AccountID       string     `json:"accountId"`
	PrincipalARN    string     `json:"principalArn,omitempty"`
	PrincipalID     string     `json:"principalId,omitempty"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
}

type Registry struct {
	credentials map[string]Credential
}

func NewRegistry(defaultAccountID string, seed ...Credential) *Registry {
	registry := &Registry{
		credentials: map[string]Credential{},
	}
	cred := Credential{
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		AccountID:       defaultAccountID,
		PrincipalARN:    fmt.Sprintf("arn:aws:iam::%s:user/local-test", defaultAccountID),
		PrincipalID:     "LOCALTEST",
	}
	if len(seed) > 0 {
		cred = seed[0]
		if cred.AccessKeyID == "" {
			cred.AccessKeyID = "test"
		}
		if cred.SecretAccessKey == "" {
			cred.SecretAccessKey = "test"
		}
		if cred.AccountID == "" {
			cred.AccountID = defaultAccountID
		}
		if cred.PrincipalARN == "" {
			cred.PrincipalARN = fmt.Sprintf("arn:aws:iam::%s:user/local-test", cred.AccountID)
		}
		if cred.PrincipalID == "" {
			cred.PrincipalID = "LOCALTEST"
		}
	}
	registry.Put(cred)
	return registry
}

func (r *Registry) Put(cred Credential) {
	if r.credentials == nil {
		r.credentials = map[string]Credential{}
	}
	r.credentials[cred.AccessKeyID] = cred
}

func (r *Registry) Get(accessKeyID string) (Credential, bool) {
	cred, ok := r.credentials[accessKeyID]
	return cred, ok
}

func (r *Registry) LoadFile(path string) error {
	if path == "" {
		return nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var direct []Credential
	if err := json.Unmarshal(raw, &direct); err == nil {
		for _, cred := range direct {
			r.Put(cred)
		}
		return nil
	}

	var wrapped struct {
		Credentials []Credential `json:"credentials"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return err
	}
	for _, cred := range wrapped.Credentials {
		r.Put(cred)
	}
	return nil
}

type Authenticator interface {
	Authenticate(context.Context, *http.Request, []byte) (protocol.Identity, error)
}

type Mode string

const (
	ModeStrict Mode = "strict"
	ModeBypass Mode = "bypass"
)

type Service struct {
	mode           Mode
	clock          clock.Clock
	registry       *Registry
	allowedRegions map[string]struct{}
	fallbackRegion string
	accountID      string
}

func NewService(mode string, clk clock.Clock, registry *Registry, allowedRegions []string, fallbackRegion, accountID string) *Service {
	allowed := make(map[string]struct{}, len(allowedRegions))
	for _, region := range allowedRegions {
		allowed[region] = struct{}{}
	}
	return &Service{
		mode:           Mode(mode),
		clock:          clk,
		registry:       registry,
		allowedRegions: allowed,
		fallbackRegion: fallbackRegion,
		accountID:      accountID,
	}
}

func (s *Service) Authenticate(_ context.Context, r *http.Request, body []byte) (protocol.Identity, error) {
	if s.mode == ModeBypass {
		return protocol.Identity{
			AccessKeyID:  "bypass",
			AccountID:    s.accountID,
			PrincipalARN: fmt.Sprintf("arn:aws:iam::%s:user/bypass", s.accountID),
			PrincipalID:  "BYPASS",
			Region:       s.fallbackRegion,
			Service:      "sqs",
		}, nil
	}

	if authz := r.Header.Get("Authorization"); authz != "" {
		return s.authenticateHeaderSigned(r, body, authz)
	}

	if algo := r.URL.Query().Get("X-Amz-Algorithm"); algo != "" {
		return s.authenticateQuerySigned(r, body)
	}

	return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("missing signature"))
}

func (s *Service) authenticateHeaderSigned(r *http.Request, body []byte, authz string) (protocol.Identity, error) {
	if !strings.HasPrefix(authz, "AWS4-HMAC-SHA256 ") {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("unsupported algorithm"))
	}

	fields := parseAuthFields(strings.TrimPrefix(authz, "AWS4-HMAC-SHA256 "))
	credentialValue := fields["Credential"]
	signedHeadersValue := fields["SignedHeaders"]
	signatureValue := fields["Signature"]
	if credentialValue == "" || signedHeadersValue == "" || signatureValue == "" {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("malformed authorization header"))
	}

	scope, err := parseCredentialScope(credentialValue)
	if err != nil {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(err)
	}

	cred, identity, err := s.lookupIdentity(scope.AccessKeyID, r.Header.Get("X-Amz-Security-Token"))
	if err != nil {
		return protocol.Identity{}, err
	}

	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("missing X-Amz-Date"))
	}
	requestTime, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(err)
	}
	if err := s.validateScope(scope, requestTime); err != nil {
		return protocol.Identity{}, err
	}

	canonicalRequest, err := buildCanonicalRequest(r, body, strings.Split(signedHeadersValue, ";"), false)
	if err != nil {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(err)
	}
	stringToSign := buildStringToSign(requestTime, scope, canonicalRequest)
	expectedSig := signStringToSign(cred.SecretAccessKey, scope, stringToSign)
	if subtle.ConstantTimeCompare([]byte(expectedSig), []byte(signatureValue)) != 1 {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("signature mismatch"))
	}

	identity.Region = scope.Region
	identity.Service = scope.Service
	return identity, nil
}

func (s *Service) authenticateQuerySigned(r *http.Request, body []byte) (protocol.Identity, error) {
	values := r.URL.Query()
	if values.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("unsupported presign algorithm"))
	}

	scope, err := parseCredentialScope(values.Get("X-Amz-Credential"))
	if err != nil {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(err)
	}
	cred, identity, err := s.lookupIdentity(scope.AccessKeyID, values.Get("X-Amz-Security-Token"))
	if err != nil {
		return protocol.Identity{}, err
	}

	requestTime, err := time.Parse("20060102T150405Z", values.Get("X-Amz-Date"))
	if err != nil {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(err)
	}
	if err := s.validateScope(scope, requestTime); err != nil {
		return protocol.Identity{}, err
	}

	expiresRaw := values.Get("X-Amz-Expires")
	if expiresRaw == "" {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("missing X-Amz-Expires"))
	}
	expires, err := time.ParseDuration(expiresRaw + "s")
	if err != nil {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(err)
	}
	if s.clock.Now().UTC().After(requestTime.Add(expires)) {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("presigned request expired"))
	}

	signedHeaders := strings.Split(values.Get("X-Amz-SignedHeaders"), ";")
	canonicalRequest, err := buildCanonicalRequest(r, body, signedHeaders, true)
	if err != nil {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(err)
	}
	stringToSign := buildStringToSign(requestTime, scope, canonicalRequest)
	expectedSig := signStringToSign(cred.SecretAccessKey, scope, stringToSign)
	if subtle.ConstantTimeCompare([]byte(expectedSig), []byte(values.Get("X-Amz-Signature"))) != 1 {
		return protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("signature mismatch"))
	}

	identity.Region = scope.Region
	identity.Service = scope.Service
	return identity, nil
}

func (s *Service) lookupIdentity(accessKeyID, sessionToken string) (Credential, protocol.Identity, error) {
	cred, ok := s.registry.Get(accessKeyID)
	if !ok {
		return Credential{}, protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("unknown access key"))
	}
	if cred.ExpiresAt != nil && s.clock.Now().UTC().After(cred.ExpiresAt.UTC()) {
		return Credential{}, protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("credential expired"))
	}
	if cred.SessionToken != "" && cred.SessionToken != sessionToken {
		return Credential{}, protocol.Identity{}, apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("session token mismatch"))
	}
	return cred, protocol.Identity{
		AccessKeyID:  cred.AccessKeyID,
		AccountID:    cred.AccountID,
		PrincipalARN: cred.PrincipalARN,
		PrincipalID:  cred.PrincipalID,
		SessionToken: cred.SessionToken,
	}, nil
}

func (s *Service) validateScope(scope credentialScope, requestTime time.Time) error {
	if scope.Service != "sqs" {
		return apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("unexpected service %q", scope.Service))
	}
	if len(s.allowedRegions) != 0 {
		if _, ok := s.allowedRegions[scope.Region]; !ok {
			return apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("unexpected region %q", scope.Region))
		}
	}
	now := s.clock.Now().UTC()
	if requestTime.Before(now.Add(-15*time.Minute)) || requestTime.After(now.Add(15*time.Minute)) {
		return apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("request time skew too large"))
	}
	if requestTime.Format("20060102") != scope.Date {
		return apierrors.ErrInvalidSecurity.WithCause(fmt.Errorf("credential scope date mismatch"))
	}
	return nil
}

type credentialScope struct {
	AccessKeyID string
	Date        string
	Region      string
	Service     string
	Terminal    string
}

func parseCredentialScope(raw string) (credentialScope, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 5 {
		return credentialScope{}, fmt.Errorf("invalid credential scope")
	}
	return credentialScope{
		AccessKeyID: parts[0],
		Date:        parts[1],
		Region:      parts[2],
		Service:     parts[3],
		Terminal:    parts[4],
	}, nil
}

func parseAuthFields(raw string) map[string]string {
	fields := map[string]string{}
	for field := range strings.SplitSeq(raw, ",") {
		field = strings.TrimSpace(field)
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			continue
		}
		fields[parts[0]] = parts[1]
	}
	return fields
}

func buildCanonicalRequest(r *http.Request, body []byte, signedHeaders []string, presigned bool) (string, error) {
	path := r.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	queryString := buildCanonicalQueryString(r.URL.Query(), presigned)
	headers, err := buildCanonicalHeaders(r, signedHeaders)
	if err != nil {
		return "", err
	}
	signedHeadersValue := strings.Join(signedHeaders, ";")

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = r.URL.Query().Get("X-Amz-Content-Sha256")
	}
	if payloadHash == "" {
		sum := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(sum[:])
	}

	var builder strings.Builder
	builder.WriteString(r.Method)
	builder.WriteString("\n")
	builder.WriteString(path)
	builder.WriteString("\n")
	builder.WriteString(queryString)
	builder.WriteString("\n")
	builder.WriteString(headers)
	builder.WriteString("\n")
	builder.WriteString(signedHeadersValue)
	builder.WriteString("\n")
	builder.WriteString(payloadHash)
	return builder.String(), nil
}

func buildCanonicalHeaders(r *http.Request, signedHeaders []string) (string, error) {
	var builder strings.Builder
	for _, name := range signedHeaders {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" {
			continue
		}
		var value string
		if name == "host" {
			value = r.Host
		} else {
			value = r.Header.Get(name)
		}
		if value == "" {
			return "", fmt.Errorf("missing signed header %q", name)
		}
		builder.WriteString(name)
		builder.WriteString(":")
		builder.WriteString(canonicalizeHeaderValue(value))
		builder.WriteString("\n")
	}
	return builder.String(), nil
}

func buildCanonicalQueryString(values url.Values, presigned bool) string {
	type pair struct {
		key   string
		value string
	}
	pairs := make([]pair, 0, len(values))
	for key, rawValues := range values {
		for _, value := range rawValues {
			if presigned && key == "X-Amz-Signature" {
				continue
			}
			pairs = append(pairs, pair{key: awsPercentEncode(key), value: awsPercentEncode(value)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key == pairs[j].key {
			return pairs[i].value < pairs[j].value
		}
		return pairs[i].key < pairs[j].key
	})
	parts := make([]string, 0, len(pairs))
	for _, item := range pairs {
		parts = append(parts, item.key+"="+item.value)
	}
	return strings.Join(parts, "&")
}

func buildStringToSign(requestTime time.Time, scope credentialScope, canonicalRequest string) string {
	hash := sha256.Sum256([]byte(canonicalRequest))
	return strings.Join([]string{
		"AWS4-HMAC-SHA256",
		requestTime.UTC().Format("20060102T150405Z"),
		scope.Date + "/" + scope.Region + "/" + scope.Service + "/" + scope.Terminal,
		hex.EncodeToString(hash[:]),
	}, "\n")
}

func signStringToSign(secret string, scope credentialScope, stringToSign string) string {
	dateKey := hmacSHA256([]byte("AWS4"+secret), scope.Date)
	regionKey := hmacSHA256(dateKey, scope.Region)
	serviceKey := hmacSHA256(regionKey, scope.Service)
	signingKey := hmacSHA256(serviceKey, scope.Terminal)
	signature := hmacSHA256(signingKey, stringToSign)
	return hex.EncodeToString(signature)
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func canonicalizeHeaderValue(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func awsPercentEncode(raw string) string {
	const hexChars = "0123456789ABCDEF"
	var builder strings.Builder
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.' || ch == '~' {
			builder.WriteByte(ch)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hexChars[ch>>4])
		builder.WriteByte(hexChars[ch&0x0F])
	}
	return builder.String()
}
