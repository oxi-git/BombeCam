package bridge

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/Fever-r/BombeCam/pkg/serverkey"
)

// Osaio client identity and default addresses. If Osaio changes its API,
// this file and signaling.go are the places to update. The server key and the
// app ID are not here: the key comes from a serverkey.Provider, the app ID
// from AppID (appid.go).
const (
	globalBase = "https://global.osaio.net"
	defaultWeb = "https://app-eu.osaio.net"
	defaultWS  = "wss://ali-wss-eu.osaio.net/ws"
	userAgent  = "OSAIO_ANDROID_4.7.2_688"
)

// ErrServerKey means a request was not sent because no usable server key is
// configured. It wraps serverkey.ErrNoKey.
var ErrServerKey = serverkey.ErrNoKey

func hmacHex(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// sign returns the request signature for msg using the configured key.
func (c *Cloud) sign(msg string) (string, error) {
	if c.keys == nil {
		return "", fmt.Errorf("%w: no key source", ErrServerKey)
	}
	key, err := c.keys.Key()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(hmacHex(key, msg))), nil
}

type Cloud struct {
	Web          string
	WsURL        string
	Country      string
	PhoneCode    string
	UID          string
	APIToken     string
	GlobalBase   string
	TimezoneName string
	ZoneOffset   float64
	hc           *http.Client
	keys         serverkey.Provider
}

// NewCloud creates an Osaio cloud client that signs its requests with the
// key from keys.
func NewCloud(country, phoneCode string, keys serverkey.Provider) *Cloud {
	web := defaultWeb
	if v := os.Getenv("OSAIO_DEFAULT_WEB"); v != "" {
		web = v
	}
	ws := defaultWS
	if v := os.Getenv("OSAIO_DEFAULT_WS"); v != "" {
		ws = v
	}
	gBase := globalBase
	if v := os.Getenv("OSAIO_GLOBAL_BASE"); v != "" {
		gBase = v
	}

	tzName, offset := time.Now().Zone()
	zoneHours := float64(offset) / 3600.0
	if v := os.Getenv("OSAIO_TIMEZONE"); v != "" {
		tzName = v
	}
	if v := os.Getenv("OSAIO_ZONE_OFFSET"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			zoneHours = f
		}
	}

	return &Cloud{
		Web:          web,
		WsURL:        ws,
		Country:      country,
		PhoneCode:    phoneCode,
		GlobalBase:   gBase,
		TimezoneName: tzName,
		ZoneOffset:   zoneHours,
		hc:           &http.Client{Timeout: 20 * time.Second},
		keys:         keys,
	}
}

var (
	// ErrInvalidCredentials indicates authentication failure with the vendor cloud.
	ErrInvalidCredentials = errors.New("invalid account credentials")
	// ErrUpstreamServiceUnavailable indicates vendor cloud 502/503/504 errors.
	ErrUpstreamServiceUnavailable = errors.New("vendor cloud service unavailable")
	// ErrUpstreamMalformedData indicates non-JSON or invalid data from vendor.
	ErrUpstreamMalformedData = errors.New("upstream service returned malformed response")
)

// UpstreamHTTPError represents an HTTP error response from the vendor cloud.
type UpstreamHTTPError struct {
	StatusCode int
}

func (e *UpstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream vendor returned HTTP %d", e.StatusCode)
}

func (c *Cloud) do(method, urlStr string, headers map[string]string, body []byte) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, urlStr, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
		return nil, fmt.Errorf("%w: %w", ErrUpstreamServiceUnavailable, &UpstreamHTTPError{StatusCode: resp.StatusCode})
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCredentials, &UpstreamHTTPError{StatusCode: resp.StatusCode})
	}
	if resp.StatusCode >= 400 {
		return nil, &UpstreamHTTPError{StatusCode: resp.StatusCode}
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	var j map[string]any
	if err := json.Unmarshal(raw, &j); err != nil {
		// Strictly avoid leaking raw response bodies or canary secrets
		return nil, fmt.Errorf("%w: invalid JSON", ErrUpstreamMalformedData)
	}
	return j, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// h1 builds the headers for requests made before sign-in.
func (c *Cloud) h1() (map[string]string, error) {
	appID, err := AppID()
	if err != nil {
		return nil, err
	}
	ts := time.Now().Unix()
	sig, err := c.sign(fmt.Sprintf("%s%d", appID, ts))
	if err != nil {
		return nil, err
	}
	return map[string]string{"appid": appID, "timestamp": strconv.FormatInt(ts, 10), "sign": sig, "ApiSignType": "1"}, nil
}

// h2 builds the headers for requests made with a signed-in session.
func (c *Cloud) h2() (map[string]string, error) {
	appID, err := AppID()
	if err != nil {
		return nil, err
	}
	ts := time.Now().Unix()
	sig, err := c.sign(fmt.Sprintf("%s%d%s%s", appID, ts, c.UID, c.APIToken))
	if err != nil {
		return nil, err
	}
	return map[string]string{"appid": appID, "timestamp": strconv.FormatInt(ts, 10),
		"sign": sig, "ApiSignType": "2", "uid": c.UID, "api-token": c.APIToken, "timeout": "10"}, nil
}

func (c *Cloud) GetBaseURL(account string) {
	h, err := c.h1()
	if err != nil {
		fmt.Println("[get-baseurl] skipped:", err)
		return
	}
	h["BaseUrlName"] = "global"
	baseURL := c.GlobalBase
	if baseURL == "" {
		baseURL = globalBase
	}
	u := baseURL + "/v2/account/get-baseurl?" + url.Values{"account": {account}, "country": {c.Country}}.Encode()
	j, err := c.do("GET", u, h, nil)
	if err != nil {
		fmt.Println("[get-baseurl] skipped:", err)
		return
	}
	if data, ok := j["data"].(map[string]any); ok {
		if w, ok := data["web"].(string); ok && w != "" {
			c.Web = trimV2(w)
		}
		if ws, ok := data["ws"].(string); ok && ws != "" {
			c.WsURL = ws
		}
		fmt.Printf("[get-baseurl] region=%v web=%s ws=%s\n", data["region"], c.Web, c.WsURL)
	}
}

func trimV2(s string) string {
	if len(s) > 3 && s[len(s)-3:] == "/v2" {
		return s[:len(s)-3]
	}
	return s
}

// Clone returns a copy with the same endpoints and settings and no login,
// for signing in again without changing a Cloud other goroutines use.
func (c *Cloud) Clone() *Cloud {
	n := *c
	n.UID, n.APIToken = "", ""
	return &n
}

func (c *Cloud) Login(account, password string) error {
	pwd := fmt.Sprintf("%x", md5.Sum([]byte(password)))

	tzName := c.TimezoneName
	zoneOffset := c.ZoneOffset
	if tzName == "" {
		name, offset := time.Now().Zone()
		tzName = name
		zoneOffset = float64(offset) / 3600.0
	}

	body, _ := json.Marshal(map[string]any{
		"account": account, "country": c.Country, "password": pwd,
		"phone_brand": "OSAIO-GO", "phone_code": c.PhoneCode,
		"timezone_name": tzName, "zone": zoneOffset,
	})
	h, err := c.h1()
	if err != nil {
		return err
	}
	h["Content-Type"] = "application/json; charset=UTF-8"
	h["BaseUrlName"] = "web"
	j, err := c.do("POST", c.Web+"/v2/login/login", h, body)
	if err != nil {
		return err
	}
	if code, _ := j["code"].(float64); code != 1000 {
		return fmt.Errorf("%w: login code=%v msg=%v", ErrInvalidCredentials, j["code"], j["msg"])
	}
	data, _ := j["data"].(map[string]any)
	c.UID, _ = data["uid"].(string)
	c.APIToken, _ = data["api_token"].(string)
	if c.UID == "" || c.APIToken == "" {
		return fmt.Errorf("%w: login reply had no account ID or token", ErrUpstreamMalformedData)
	}
	fmt.Printf("[login] OK uid=%s\n", c.UID)
	return nil
}

type Device struct {
	UUID   string `json:"uuid"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Model  int    `json:"model_type"`
	Online int    `json:"online"`
	// Config is the camera's settings as the Osaio device list reports them
	// (e.g. MotionDetectSW, SoundDetectSW).
	Config map[string]any `json:"device_config,omitempty"`
}

func (c *Cloud) DeviceList() ([]Device, error) {
	h, err := c.h2()
	if err != nil {
		return nil, err
	}
	h["BaseUrlName"] = "web_baseurl"
	j, err := c.do("GET", c.Web+"/v2/device/list?per_page=100&page=1", h, nil)
	if err != nil {
		return nil, err
	}
	if code, _ := j["code"].(float64); code != 1000 {
		return nil, fmt.Errorf("device/list code=%v", j["code"])
	}
	return parseDeviceList(j["data"])
}

// parseDeviceList reads the device/list reply's data. An account with no
// cameras comes back as "data": null (seen after a camera was reset and
// paired again); that is an empty list, not an error.
func parseDeviceList(data any) ([]Device, error) {
	var list any
	switch d := data.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		list = d["data"]
	case []any:
		list = d
	default:
		return nil, fmt.Errorf("%w: device list", ErrUpstreamMalformedData)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return nil, fmt.Errorf("%w: device list", ErrUpstreamMalformedData)
	}
	var devs []Device
	if err := json.Unmarshal(raw, &devs); err != nil {
		return nil, fmt.Errorf("%w: device list", ErrUpstreamMalformedData)
	}
	return devs, nil
}

type VideoCall struct {
	SessionID  string `json:"session_id"`
	Expired    int64  `json:"expired"`
	DeviceICEs []ICE  `json:"device_ices"`
	UserICEs   []ICE  `json:"user_ices"`
}
type ICE struct {
	URL      string `json:"iceurl"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (c *Cloud) VideoCall(deviceID string) (*VideoCall, error) {
	h, err := c.h2()
	if err != nil {
		return nil, err
	}
	h["Content-Type"] = "application/json; charset=utf-8"
	h["BaseUrlName"] = "web_baseurl"
	body, _ := json.Marshal(map[string]any{"device_id": deviceID})
	j, err := c.do("POST", c.Web+"/v2/webrtcsession/user/videocall", h, body)
	if err != nil {
		return nil, err
	}
	if code, _ := j["code"].(float64); code != 1000 {
		return nil, fmt.Errorf("videocall code=%v", j["code"])
	}
	raw, _ := json.Marshal(j["data"])
	var vc VideoCall
	json.Unmarshal(raw, &vc)
	return &vc, nil
}

// --- Cloud recordings and storage (routes read from the Osaio app 4.9.0) ---
//
// These come from com.yrcx.xplayer.ui.repository.YRPlatformApi in base.apk of
// com.afar.osaio 4.9.0 (sha256 e015dc96...), which sends "uuid" (the camera
// id), "uid" and a bind_type. The SD card itself is not here: the app reads
// it over P2P (YRCXSDK, DP "sdCardState"/FormatInfo), so nothing in the cloud
// API lists files on the camera's card.
// CloudRecordingDay is one day that has cloud recordings.
type CloudRecordingDay struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// ListCloudRecordingDays asks file/info/playback which days of a month have
// cloud recordings (month is YYYY-MM).
func (c *Cloud) ListCloudRecordingDays(deviceID, uid, month string, bindType int) ([]CloudRecordingDay, error) {
	h, err := c.h2()
	if err != nil {
		return nil, err
	}
	h["BaseUrlName"] = "web_baseurl"
	q := url.Values{
		"uuid":      {deviceID},
		"uid":       {uid},
		"file_date": {month},
		"file_time": {"0"},
		"bind_type": {strconv.Itoa(bindType)},
	}
	j, err := c.do("GET", c.Web+"/v2/file/info/playback?"+q.Encode(), h, nil)
	if err != nil {
		return nil, err
	}
	if code, _ := j["code"].(float64); code != 1000 {
		return nil, fmt.Errorf("file/info/playback code=%v msg=%v", j["code"], j["msg"])
	}
	raw, _ := json.Marshal(j["data"])
	var days []CloudRecordingDay
	if err := json.Unmarshal(raw, &days); err != nil {
		return nil, nil
	}
	return days, nil
}

// GetPlaybackURL asks the cloud for a signed URL to one recorded file.
// day is YYYY-MM-DD and startTime unix seconds, as the app sends them.
func (c *Cloud) GetPlaybackURL(deviceID, uid, day string, startTime int64, bindType int) (string, error) {
	h, err := c.h2()
	if err != nil {
		return "", err
	}
	h["BaseUrlName"] = "web_baseurl"
	q := url.Values{
		"deviceid":   {deviceID},
		"userid":     {uid},
		"starttime":  {strconv.FormatInt(startTime, 10)},
		"file_type":  {"video"},
		"expiration": {"3600"},
		"bindtype":   {strconv.Itoa(bindType)},
	}
	j, err := c.do("GET", c.Web+"/v2/fileget_presignurl?"+q.Encode(), h, nil)
	if err != nil {
		return "", err
	}
	if code, _ := j["code"].(float64); code != 1000 {
		return "", fmt.Errorf("fileget_presignurl code=%v msg=%v", j["code"], j["msg"])
	}
	raw, _ := json.Marshal(j["data"])
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", fmt.Errorf("no playback URL in response")
	}
	for _, k := range []string{"url", "play_url", "playUrl"} {
		if u, ok := d[k].(string); ok && u != "" {
			return u, nil
		}
	}
	return "", fmt.Errorf("no playback URL in response")
}
