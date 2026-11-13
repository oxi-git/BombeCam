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

// --- SD Card / Playback API (Osaio/Reolink compatible) ---

// SDCardInfo holds the SD card status from the camera.
type SDCardInfo struct {
	TotalSpace  int64  `json:"total_space"`  // bytes
	FreeSpace   int64  `json:"free_space"`   // bytes
	UsedSpace   int64  `json:"used_space"`   // bytes
	Status      int    `json:"status"`       // 0=normal, 1=error, 2=no card, etc.
	StatusText  string `json:"status_text"`  // human readable
}

// GetSDCardInfo queries the camera's SD card status.
func (c *Cloud) GetSDCardInfo(deviceID string) (*SDCardInfo, error) {
	h, err := c.h2()
	if err != nil {
		return nil, err
	}
	h["BaseUrlName"] = "web_baseurl"
	body, _ := json.Marshal(map[string]any{"device_id": deviceID})
	j, err := c.do("POST", c.Web+"/v2/device/sdcard/info", h, body)
	if err != nil {
		return nil, err
	}
	if code, _ := j["code"].(float64); code != 1000 {
		return nil, fmt.Errorf("sdcard/info code=%v msg=%v", j["code"], j["msg"])
	}
	data, _ := j["data"].(map[string]any)
	info := &SDCardInfo{}
	if v, ok := data["total_space"].(float64); ok {
		info.TotalSpace = int64(v)
	}
	if v, ok := data["free_space"].(float64); ok {
		info.FreeSpace = int64(v)
	}
	if v, ok := data["used_space"].(float64); ok {
		info.UsedSpace = int64(v)
	}
	if v, ok := data["status"].(float64); ok {
		info.Status = int(v)
	}
	if v, ok := data["status_text"].(string); ok {
		info.StatusText = v
	}
	return info, nil
}

// RecordFile represents a single recording file on the SD card.
type RecordFile struct {
	StartTime  int64  `json:"start_time"`   // unix timestamp
	EndTime    int64  `json:"end_time"`     // unix timestamp
	Size       int64  `json:"size"`         // bytes
	Type       int    `json:"type"`         // 1=manual, 2=motion, 3=alarm, etc.
	Path       string `json:"path"`         // file path on SD
	URL        string `json:"url"`          // playback/download URL if provided
	DeviceID   string `json:"device_id"`    // camera UUID
}

// ListRecordings queries the SD card for recordings in a time range.
// startTime, endTime are unix timestamps (0 = no limit).
func (c *Cloud) ListRecordings(deviceID string, startTime, endTime int64) ([]RecordFile, error) {
	h, err := c.h2()
	if err != nil {
		return nil, err
	}
	h["BaseUrlName"] = "web_baseurl"
	body, _ := json.Marshal(map[string]any{
		"device_id":  deviceID,
		"start_time": startTime,
		"end_time":   endTime,
		"page":       1,
		"per_page":   100,
	})
	j, err := c.do("POST", c.Web+"/v2/device/record/list", h, body)
	if err != nil {
		return nil, err
	}
	if code, _ := j["code"].(float64); code != 1000 {
		return nil, fmt.Errorf("record/list code=%v msg=%v", j["code"], j["msg"])
	}
	data, _ := j["data"].(map[string]any)
	listRaw, ok := data["data"].([]any)
	if !ok {
		return nil, nil
	}
	var files []RecordFile
	raw, _ := json.Marshal(listRaw)
	json.Unmarshal(raw, &files)
	return files, nil
}

// GetPlaybackURL requests a playback URL for a specific recording file.
// Returns a signed URL (usually HTTPS) that can be played directly.
func (c *Cloud) GetPlaybackURL(deviceID, filePath string) (string, error) {
	h, err := c.h2()
	if err != nil {
		return "", err
	}
	h["BaseUrlName"] = "web_baseurl"
	body, _ := json.Marshal(map[string]any{"device_id": deviceID, "file_path": filePath})
	j, err := c.do("POST", c.Web+"/v2/device/playback", h, body)
	if err != nil {
		return "", err
	}
	if code, _ := j["code"].(float64); code != 1000 {
		return "", fmt.Errorf("playback code=%v msg=%v", j["code"], j["msg"])
	}
	data, _ := j["data"].(map[string]any)
	if url, ok := data["url"].(string); ok {
		return url, nil
	}
	if url, ok := data["playback_url"].(string); ok {
		return url, nil
	}
	return "", fmt.Errorf("no playback URL in response")
}
