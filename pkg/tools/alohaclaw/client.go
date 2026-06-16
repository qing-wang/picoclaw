package alohaclawtools

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// Built-in CA certificate for AlohaClaw MQTT broker TLS verification.
// Mirrors the BuiltInTrustChainPem constant in the C# AlohaClaw SDK.
const builtInCAPEM = `-----BEGIN CERTIFICATE-----
MIIFBTCCAu2gAwIBAgIUX2EEhBzWlh8iWdoevD4QajYFkVYwDQYJKoZIhvcNAQEL
BQAwEjEQMA4GA1UEAwwHTVFUVC1DQTAeFw0yNjAyMjQwNzQ0MDNaFw0zNjAyMjIw
NzQ0MDNaMBIxEDAOBgNVBAMMB01RVFQtQ0EwggIiMA0GCSqGSIb3DQEBAQUAA4IC
DwAwggIKAoICAQCcqWnimKZbnYzLeiyUUvVmgnBMa2SSVnR/j2LC4ynzxLJWhLo2
gHiksWk4n9BsV23quphLWv0bY1xU5+rSSvqNVClDpBGPpzdeQKb3my2fvgoLEQTt
jddhUVXqpwr7MgUyvHu1xzZqlJJoTRbcbbNqHHb1Z4k7k6kywiBXyraXkxJL5I6r
drjZSfg9xG7dXFxqTyyovxGPGX2IgkDKuUIVFxZa//jYPT8rkIvt8bdWn1gInLqv
U/nkR4iG6TX+QRWaKV/Ig2jTUPe6g6dHC/GPDM9r2wnaLGLpgtdSMtjy1Mg+s/He
HGpDOHxQlbxxHjt25c/PNWskD3OBq6p8sZnUjtUASFLBnbB0DdOwQ4FG2FSvjJlc
1eDV/ZY7+6GQdLTsXaP7EAgRiJmbUnszLL9ZGFfR8rP4H5NtTmNrnV605sMb0frV
Qt5hc/6fSjiSDOpMiaxfRoVGv+3I7fvQL9v3WH1EqZq+9UW89qvEJHh4Xsnzd2nK
FxhmbDvv6lptKiNnTNT0UgSt/kMhgj5m6sHim5JBT1W4APN5NV1IUx6/5/Fj5yoi
OH2RW9eQKgHWgvazMvuXNU/cbOWXPqla95TVJZHeSl0uMi4UvKp/SCdgBWiMDQFV
S/91m7J/LytBdAQCyxBFRDJjdre+3wSR+TIU2WcfdUMmQPLt63WnDTaeSwIDAQAB
o1MwUTAdBgNVHQ4EFgQUu20+02gDd1F5oT9S21FF5pr/SmMwHwYDVR0jBBgwFoAU
u20+02gDd1F5oT9S21FF5pr/SmMwDwYDVR0TAQH/BAUwAwEB/zANBgkqhkiG9w0B
AQsFAAOCAgEAfwEOul6PaUXx2n97Wb1rQqoLOIN7mMeNwJHeEkLNk7GoyuJaaMVc
d226GxZ2uMHGrtQ9QnuKsJhvj7sLK443ISc32IbtH/vpJxTiguW3tG3q8RckBDHt
HxaoIXMzawMgFgfwQezxOK63yCcDpCzBqb4ypMUXKHxYMkS/LxgzJlktX6sMCSmz
mp+YcNIYosmdlWlQwY/uf94LSNgFTFUkL54rkXW4PXCV4wAOmFr8ZtSd4FdJWIBN
EDsNVeoY55Yf5o8zu9QoBfT1fsX0rcoEhun7bSSP3OD5SUe8ocqSMRtTtey5G/OM
mTMF97kpDVc/5/yuka79j+CRkFoTarzV7FrpdJbcKMngYgSXej6R+i2pw+Eb8wNG
FXrrrLDXBcnmuACL0C/KLOxk2TuqqDhEB0hAap6G2DgaRIh4m4pBjTAxPw860BJU
a18vSJWzuvJZu2HOrvaFwnOEV1V6E/fDE5GEjrKf363eQsPJuhCA1wTik87d/vhV
gQ3Ek97OUnUvVwHlQExH8uzPxGDHOm3i89+Wq0MpKsERIjEGALVjCgf8qUdYMlYk
l+Rr5VBCHoWmsXvgdKDnt5RtK0tI8Z0U7KftpoRlqHj/cKuJDlgeGJWB74BrbCj5
F79XhlaVi/LIlVZS5dnK8jkR+NCD6AY0QvELYaNYdWvzgzG8CKMow3U=
-----END CERTIFICATE-----`

type mqttPayload struct {
	From      string `json:"from"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	RequestID string `json:"request_id"`
}

type alohaClawClient struct {
	brokerIP string
	port     int
	botID    string
	password string

	mu          sync.Mutex
	mqttClient  mqtt.Client
	pendingReqs map[string]chan string
}

func newAlohaClawClient(brokerIP string, port int, botID, password string) (*alohaClawClient, error) {
	c := &alohaClawClient{
		brokerIP:    brokerIP,
		port:        port,
		botID:       botID,
		password:    password,
		pendingReqs: make(map[string]chan string),
	}
	if err := c.connect(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *alohaClawClient) makeTLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(builtInCAPEM))

	// Verify chain against built-in CA but ignore hostname mismatch,
	// matching C# AlohaClaw SDK's ValidateServerCertificate behaviour.
	return &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("no peer certificate received")
			}
			opts := x509.VerifyOptions{
				Roots:         pool,
				Intermediates: x509.NewCertPool(),
			}
			for _, cert := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(cert)
			}
			if _, err := cs.PeerCertificates[0].Verify(opts); err != nil {
				return fmt.Errorf("AlohaClaw TLS chain verification failed: %w", err)
			}
			return nil
		},
	}
}

func (c *alohaClawClient) connect() error {
	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcps://%s:%d", c.brokerIP, c.port)).
		SetClientID(c.botID).
		SetUsername(c.botID).
		SetPassword(c.password).
		SetKeepAlive(30 * time.Second).
		SetAutoReconnect(true).
		SetTLSConfig(c.makeTLSConfig()).
		SetOnConnectHandler(func(cl mqtt.Client) {
			c.resubscribe(cl)
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			logger.WarnCF("alohaclaw", "MQTT connection lost", map[string]any{"error": err.Error()})
		})

	cl := mqtt.NewClient(opts)
	tok := cl.Connect()
	if !tok.WaitTimeout(15 * time.Second) {
		return fmt.Errorf("MQTT connect timeout")
	}
	if err := tok.Error(); err != nil {
		return fmt.Errorf("MQTT connect to %s:%d: %w", c.brokerIP, c.port, err)
	}
	c.mqttClient = cl
	return nil
}

// resubscribe to own topics after every (re)connect.
func (c *alohaClawClient) resubscribe(cl mqtt.Client) {
	topics := map[string]byte{
		c.botID + "_command": 1,
		c.botID + "_message": 1,
	}
	tok := cl.SubscribeMultiple(topics, c.onMessage)
	tok.Wait()
	if err := tok.Error(); err != nil {
		logger.WarnCF("alohaclaw", "MQTT resubscribe failed", map[string]any{"error": err.Error()})
	}
}

func (c *alohaClawClient) onMessage(_ mqtt.Client, msg mqtt.Message) {
	var p mqttPayload
	if err := json.Unmarshal(msg.Payload(), &p); err != nil || p.RequestID == "" {
		return
	}
	c.mu.Lock()
	ch, ok := c.pendingReqs[p.RequestID]
	c.mu.Unlock()
	if ok {
		select {
		case ch <- p.Text:
		default:
		}
	}
}

// SendCommand publishes to {targetID}_command and waits for a reply on our
// _message topic with a matching request_id. Pass waitReply=0 to fire-and-forget.
func (c *alohaClawClient) SendCommand(ctx context.Context, targetID, text string, waitReply time.Duration) (string, error) {
	reqID := uuid.New().String()

	var replyCh chan string
	if waitReply > 0 {
		replyCh = make(chan string, 1)
		c.mu.Lock()
		c.pendingReqs[reqID] = replyCh
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			delete(c.pendingReqs, reqID)
			c.mu.Unlock()
		}()
	}

	p := mqttPayload{From: c.botID, Type: "command", Text: text, RequestID: reqID}
	data, _ := json.Marshal(p)
	tok := c.mqttClient.Publish(targetID+"_command", 1, false, data)
	tok.Wait()
	if err := tok.Error(); err != nil {
		return "", fmt.Errorf("publish command: %w", err)
	}

	if waitReply <= 0 {
		return "", nil
	}

	select {
	case reply := <-replyCh:
		return reply, nil
	case <-time.After(waitReply):
		return "", fmt.Errorf("no reply from %q after %v", targetID, waitReply)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// SendMessage publishes to {targetID}_message (one-way, no reply wait).
func (c *alohaClawClient) SendMessage(targetID, text string) error {
	p := mqttPayload{From: c.botID, Type: "message", Text: text, RequestID: uuid.New().String()}
	data, _ := json.Marshal(p)
	tok := c.mqttClient.Publish(targetID+"_message", 1, false, data)
	tok.Wait()
	return tok.Error()
}

func (c *alohaClawClient) IsConnected() bool {
	return c.mqttClient != nil && c.mqttClient.IsConnected()
}

func (c *alohaClawClient) Disconnect() {
	if c.mqttClient != nil {
		c.mqttClient.Disconnect(500)
	}
}
