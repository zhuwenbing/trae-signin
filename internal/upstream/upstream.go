// Package upstream 封装 TRAE 签到、积分查询、Token 刷新等上游 API。
//
// 2026-11-18 起 TRAE 账号并入豆包飞书账号体系，登录/刷新接口发生变化：
//   - 登录改为豆包 SSO（PKCE + AuthCode），不再直接返回 refreshToken；
//   - ExchangeToken 升级到 v3，refresh 时需要设备密钥对签名（DeviceProof）；
//   - 统一宿主从 api.trae.com.cn 迁到 api.trae.cn。
//
// 签到/积分接口（/trae/api/v2/ug/...）本身未变，仍使用 Cloud-IDE-JWT。
package upstream

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"trae-signin/internal/auth"
)

const (
	// AuthHost 豆包账号融合后的统一宿主（ExchangeToken / GetUserInfo）。
	AuthHost = "https://api.trae.cn"
	// LegacyAuthHost 旧的 iCube 账号宿主，仅用于兼容旧凭证。
	LegacyAuthHost = "https://api.trae.com.cn"
	// UgHost 签到/积分接口宿主（未变）。
	UgHost = "https://api.trae.cn"

	// ClientIDTrae 新版 TRAE 客户端 ID（authConfig.TRAE.stable）。
	ClientIDTrae = "ono9krqynydwx5"
	// ClientIDSolo 旧 SOLO 客户端 ID（authConfig.SOLO.stable），旧凭证兜底用。
	ClientIDSolo = "en1oxy7wnw8j9n"

	IdeVersion     = "1.107.1"
	IdeVersionCode = "20261125"

	EpExchangeV3     = "/trae/api/v3/oauth/ExchangeToken"
	EpExchangeLegacy = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpGetUserInfo    = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus  = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim   = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage       = "/trae/api/v2/pay/ide_user_ent_usage"
)

var clientUA = "Trae/" + IdeVersion

type Client struct {
	HTTP *http.Client
}

func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		HTTP: &http.Client{Timeout: 60 * time.Second, Transport: tr},
	}
}

func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return raw, nil
}

// TokenResult 统一的换 token 结果（v3 / legacy 共用）。
type TokenResult struct {
	Token        string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
}

type tokenPayload struct {
	Token               string          `json:"Token"`
	RefreshToken        string          `json:"RefreshToken"`
	TokenExpireAt       json.RawMessage `json:"TokenExpireAt"`
	TokenExpireDuration int64           `json:"TokenExpireDuration"`
}

// parseTokenPayload 解析 Result / Data 中的 token 字段。
// TokenExpireAt 可能是时间戳、毫秒时间戳或 RFC3339 字符串。
func parseTokenPayload(p tokenPayload) *TokenResult {
	exp := parseTimeValue(p.TokenExpireAt)
	if exp == 0 && p.TokenExpireDuration > 0 {
		exp = time.Now().Unix() + p.TokenExpireDuration
	}
	return &TokenResult{Token: p.Token, RefreshToken: p.RefreshToken, ExpiresAt: exp}
}

func parseTimeValue(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		v, err := n.Int64()
		if err != nil {
			f, ferr := n.Float64()
			if ferr != nil {
				return 0
			}
			v = int64(f)
		}
		if v > 1e12 { // 毫秒
			v /= 1000
		}
		return v
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Unix()
		}
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			if v > 1e12 {
				v /= 1000
			}
			return v
		}
	}
	return 0
}

func clientIDFor(a *auth.Auth) string {
	if strings.TrimSpace(a.ClientID) != "" {
		return a.ClientID
	}
	if strings.TrimSpace(a.DevicePrivateKey) != "" {
		return ClientIDTrae
	}
	return ClientIDSolo
}

func platformCode(clientID string) string {
	if clientID == ClientIDSolo {
		return "SOLO_PC"
	}
	return "IDE_PC"
}

// deviceInfo 构造 v3 ExchangeToken 需要的设备信息（与官方客户端一致）。
func deviceInfo(a *auth.Auth, clientID string) map[string]any {
	host, _ := os.Hostname()
	return map[string]any{
		"DeviceID":        a.DeviceID,
		"MachineID":       a.MachineID,
		"PlatformCode":    platformCode(clientID),
		"DeviceType":      "PC",
		"DeviceName":      host,
		"DeviceModel":     "PC",
		"ClientVersion":   IdeVersion,
		"DevicePublicKey": a.DevicePublicKey,
		"DeviceBrand":     osBrand(),
		"DeviceCPU":       runtime.GOARCH,
		"OSInfo":          osBrand(),
		"OSVersion":       osVersion(),
	}
}

func osBrand() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	default:
		return "Linux"
	}
}

func osVersion() string {
	if runtime.GOOS == "windows" {
		return "10.0.22631"
	}
	if v := os.Getenv("OSTYPE"); v != "" {
		return v
	}
	return runtime.GOOS
}

// signDeviceProof 用设备私钥对请求签名：
// ECDSA(P-256, SHA256) over "METHOD\nPATH\nClientID\nRefreshToken\nTimestamp\nNonce"。
func signDeviceProof(privPEM, method, path, clientID, refreshToken string) (sig string, ts int64, nonce string, err error) {
	block, _ := pem.Decode([]byte(privPEM))
	if block == nil {
		return "", 0, "", fmt.Errorf("device private key: invalid PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", 0, "", fmt.Errorf("device private key: %w", err)
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return "", 0, "", fmt.Errorf("device private key: not ECDSA")
	}
	ts = time.Now().Unix()
	nb := make([]byte, 16)
	if _, err := rand.Read(nb); err != nil {
		return "", 0, "", err
	}
	nonce = hex.EncodeToString(nb)
	msg := strings.Join([]string{method, path, clientID, refreshToken, strconv.FormatInt(ts, 10), nonce}, "\n")
	digest := sha256.Sum256([]byte(msg))
	der, err := ecdsa.SignASN1(rand.Reader, ecKey, digest[:])
	if err != nil {
		return "", 0, "", err
	}
	return base64.StdEncoding.EncodeToString(der), ts, nonce, nil
}

func (c *Client) postExchange(host string, body map[string]any, token string) (*TokenResult, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchangeV3, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("x-cloudide-token", token)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result *tokenPayload `json:"Result"`
		Data   *tokenPayload `json:"Data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("exchange parse: %w", err)
	}
	p := resp.Result
	if p == nil {
		p = resp.Data
	}
	if p == nil || p.Token == "" {
		return nil, fmt.Errorf("refresh_failed: %s — re-login required", truncate(string(data), 200))
	}
	return parseTokenPayload(*p), nil
}

// ExchangeAuthCode 用豆包 SSO 回调里的 AuthCode + PKCE verifier 换取 token。
func (c *Client) ExchangeAuthCode(a *auth.Auth, authCode, codeVerifier string) (*TokenResult, error) {
	clientID := clientIDFor(a)
	body := map[string]any{
		"ClientID":     clientID,
		"ClientSecret": "",
		"AuthCode":     authCode,
		"CodeVerifier": codeVerifier,
		"DeviceInfo":   deviceInfo(a, clientID),
		"IDEVersion":   IdeVersion,
	}
	return c.postExchange(AuthHost, body, "")
}

// exchangeV3 用 refreshToken + 设备签名刷新。
func (c *Client) exchangeV3(a *auth.Auth, clientID string) (*TokenResult, error) {
	sig, ts, nonce, err := signDeviceProof(a.DevicePrivateKey, http.MethodPost, EpExchangeV3, clientID, a.RefreshToken)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"ClientID":     clientID,
		"ClientSecret": "",
		"RefreshToken": a.RefreshToken,
		"DeviceInfo":   deviceInfo(a, clientID),
		"DeviceProof": map[string]any{
			"Signature": sig,
			"Timestamp": ts,
			"Nonce":     nonce,
		},
		"IDEVersion": IdeVersion,
	}
	return c.postExchange(AuthHost, body, "")
}

// exchangeLegacy 旧版 ExchangeToken（无设备签名），用于兼容融合前的老凭证。
func (c *Client) exchangeLegacy(host, clientID, refreshToken string) (*TokenResult, error) {
	body := map[string]any{
		"ClientID":     clientID,
		"RefreshToken": refreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchangeLegacy, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", clientUA)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result *tokenPayload `json:"Result"`
		Data   *tokenPayload `json:"Data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("legacy exchange parse: %w", err)
	}
	p := resp.Result
	if p == nil {
		p = resp.Data
	}
	if p == nil || p.Token == "" {
		return nil, fmt.Errorf("refresh_failed: %s — re-login required", truncate(string(data), 200))
	}
	return parseTokenPayload(*p), nil
}

// RefreshToken 刷新 access token。
// 新凭证（带设备私钥）走 v3；老凭证走 legacy，并在两个宿主间兜底。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()

	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken — re-login required")
	}
	clientID := clientIDFor(a)

	var res *TokenResult
	var v3Err, legacyErr error
	if strings.TrimSpace(a.DevicePrivateKey) != "" {
		res, v3Err = c.exchangeV3(a, clientID)
	}
	if res == nil {
		// 老凭证可能属于旧 SOLO client，两个 client_id / 两个宿主都试一遍。
		ids := []string{clientID}
		if clientID != ClientIDSolo {
			ids = append(ids, ClientIDSolo)
		}
		for _, host := range []string{AuthHost, LegacyAuthHost} {
			for _, id := range ids {
				res, legacyErr = c.exchangeLegacy(host, id, a.RefreshToken)
				if res != nil {
					break
				}
			}
			if res != nil {
				break
			}
		}
	}
	if res == nil {
		switch {
		case v3Err != nil && legacyErr != nil:
			return fmt.Errorf("refresh failed: v3=%v; legacy=%v", v3Err, legacyErr)
		case legacyErr != nil:
			return legacyErr
		default:
			return v3Err
		}
	}

	a.AccessToken = res.Token
	if res.RefreshToken != "" {
		a.RefreshToken = res.RefreshToken
	}
	if res.ExpiresAt > 0 {
		a.ExpiresAt = res.ExpiresAt
	}
	return nil
}

// GetUserInfo 查询账号信息（融合后需要 GetUIDFusionInfo）。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname string, err error) {
	body := map[string]any{
		"ReqSource":        "IDE",
		"IDEVersion":       IdeVersion,
		"GetUIDFusionInfo": true,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, AuthHost+EpGetUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("x-cloudide-token", a.JWT())
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", err
	}
	var resp struct {
		Result struct {
			UserID     string `json:"UserID"`
			ScreenName string `json:"ScreenName"`
			NickName   string `json:"NickName"`
		} `json:"Result"`
		Data struct {
			UserInfo struct {
				UserID string `json:"UserID"`
				Name   string `json:"Name"`
			} `json:"UserInfo"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	uid = resp.Result.UserID
	nickname = resp.Result.ScreenName
	if nickname == "" {
		nickname = resp.Result.NickName
	}
	if uid == "" {
		uid = resp.Data.UserInfo.UserID
	}
	if nickname == "" {
		nickname = resp.Data.UserInfo.Name
	}
	if uid == "" {
		return "", "", fmt.Errorf("userinfo: no uid: %s", truncate(string(data), 200))
	}
	return uid, nickname, nil
}

// CheckinStatus 查询签到状态。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, UgHost+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, false, err
	}
	ugHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		Enable    bool  `json:"enable"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("checkin status parse: %w", err)
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// ErrBusy 表示设备校验未通过（服务端以 code=9074 返回）。
// 注意：文案「当前参与用户太多」是误导，真实原因是 x-device-id
// 不是客户端真实注册的设备号（随机 UUID/hex 会被直接拒绝）。
var ErrBusy = errors.New("device_rejected")

// checkinResp 签到接口的业务响应。注意：HTTP 200 不代表业务成功，
// 必须检查 code 字段（非 0 即为失败）。
type checkinResp struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// CheckinClaim 执行签到。返回 ErrBusy 表示设备校验未通过，需更换真实设备号。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	req, err := http.NewRequest(http.MethodPost, UgHost+EpCheckinClaim, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	ugHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var r checkinResp
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("claim parse: %w", err)
	}
	if r.Code == 0 {
		return nil
	}
	// 9074：设备校验未通过（文案伪装成「用户太多」）
	if r.Code == 9074 {
		return fmt.Errorf("%w: code=9074 设备号未被服务端认可", ErrBusy)
	}
	return fmt.Errorf("claim rejected: code=%d %s", r.Code, r.Message)
}

// CreditPack 单个积分包的摘要。
type CreditPack struct {
	Credits   int64  // 该包剩余额度（credits_limit - usage.credits_amount）
	Desc      string // 展示名（display_desc / group_name 兜底）
	Expire    int64  // 过期 Unix 秒，0 表示无
	IsCheckin bool   // 是否签到奖励包
}

// CreditSummary 账号积分的完整拆分。
type CreditSummary struct {
	Total    int64        // 合计
	Month    int64        // 每月登录赠送额度
	Checkin  int64        // 签到奖励额度
	MonthExp int64        // 月赠过期时间
	ChkExp   int64        // 签到包过期时间
	Packs    []CreditPack // 明细
}

// UserEntUsage 查询积分余额（合计）。保留向后兼容。
func (c *Client) UserEntUsage(a *auth.Auth) (remain int64, err error) {
	s, err := c.UserCreditSummary(a)
	if err != nil {
		return 0, err
	}
	return s.Total, nil
}

// 积分来源：账号体系与豆包合并后，余额按来源拆分。
const (
	creditSourceTrae   = 1
	creditSourceDoubao = 2
)

// UserCreditSummary 查询积分余额并拆分签到包 / 月赠包。
//
// 账号融合后 ide_user_ent_usage 返回的是 Trae + 豆包的合并余额：
//   - balance_summary[].source: 1=Trae, 2=豆包
//   - total_balance: 两个来源的合计
//
// 本工具只签到 Trae，因此展示 Trae 的余额（source=1），
// 不能直接用 total_balance，否则会把豆包积分也算进来。
func (c *Client) UserCreditSummary(a *auth.Auth) (*CreditSummary, error) {
	body := []byte(`{"require_usage":true,"req_source":1}`)
	req, err := http.NewRequest(http.MethodPost, UgHost+EpEntUsage, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ugHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		TotalBalance   *float64 `json:"total_balance"`
		BalanceSummary []struct {
			Source       int64   `json:"source"`
			TotalBalance float64 `json:"total_balance"`
		} `json:"balance_summary"`
		UsageSummary struct {
			TotalAmount float64 `json:"total_amount"`
		} `json:"usage_summary"`
		UserEntitlementPackList []struct {
			Source              int64  `json:"source"`
			DisplayDesc         string `json:"display_desc"`
			GroupName           string `json:"group_name"`
			GroupType           int64  `json:"group_type"`
			ExpireTime          int64  `json:"expire_time"`
			EntitlementBaseInfo struct {
				EntitlementID string `json:"entitlement_id"`
				Quota         struct {
					CreditsLimit float64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("ent usage parse: %w", err)
	}

	s := &CreditSummary{}
	for _, p := range resp.UserEntitlementPackList {
		// 融合账号下可能混入豆包权益包，直接跳过。
		if p.Source == creditSourceDoubao {
			continue
		}
		limit := int64(p.EntitlementBaseInfo.Quota.CreditsLimit)
		// credits_limit 是发放总额，usage.credits_amount 是已用量，
		// 两者相减才是该包剩余额度（与官方客户端一致）。
		credits := limit - int64(p.Usage.CreditsAmount)
		desc := p.DisplayDesc
		if desc == "" {
			desc = p.GroupName
		}
		isChk := p.GroupType == 1 ||
			strings.Contains(p.GroupName, "签到") ||
			strings.Contains(desc, "签到") ||
			strings.Contains(p.EntitlementBaseInfo.EntitlementID, "checkin")

		if credits <= 0 { // 免费权益壳或已用完，不计入
			continue
		}
		s.Packs = append(s.Packs, CreditPack{
			Credits: credits, Desc: desc, Expire: p.ExpireTime, IsCheckin: isChk,
		})
		if isChk {
			s.Checkin += credits
			if p.ExpireTime > s.ChkExp {
				s.ChkExp = p.ExpireTime
			}
		} else {
			s.Month += credits
			if p.ExpireTime > s.MonthExp {
				s.MonthExp = p.ExpireTime
			}
		}
	}

	// 优先取 balance_summary 中 source=1（Trae）的余额；
	// 缺失时用 total_balance 减去豆包余额兜底。
	var traeBal, doubaoBal *float64
	for i := range resp.BalanceSummary {
		b := &resp.BalanceSummary[i]
		switch b.Source {
		case creditSourceTrae:
			traeBal = &b.TotalBalance
		case creditSourceDoubao:
			doubaoBal = &b.TotalBalance
		}
	}
	switch {
	case traeBal != nil:
		s.Total = int64(*traeBal)
	case resp.TotalBalance != nil && doubaoBal != nil:
		s.Total = int64(*resp.TotalBalance - *doubaoBal)
	case resp.TotalBalance != nil:
		s.Total = int64(*resp.TotalBalance)
	case resp.UsageSummary.TotalAmount > 0:
		// 旧版响应（未融合账号）
		s.Total = int64(resp.UsageSummary.TotalAmount)
	default:
		s.Total = s.Month + s.Checkin
	}
	if s.Total < 0 {
		s.Total = 0
	}
	// balance_summary 是 Trae 的真实剩余余额，由它反推月赠额度，
	// 避免把豆包的权益包混进「月赠」列。
	if traeBal != nil || doubaoBal != nil || resp.TotalBalance != nil {
		if s.Checkin > s.Total {
			s.Checkin = s.Total
		}
		s.Month = s.Total - s.Checkin
	}
	return s, nil
}

func ugHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.JWT())
	req.Header.Set("X-User-Region", "CN")
	if a.DeviceID != "" {
		// 关键：必须是「客户端真实注册的设备号」。
		// 随机 UUID / 32 位 hex 会被服务端判为无效设备，claim 返回 code=9074
		// （文案「当前参与用户太多」是误导，真实原因是设备校验不通过）。
		// 同时用小写头名，与官方客户端一致。
		req.Header.Set("x-device-id", a.DeviceID)
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
	// 设备指纹头（官方客户端 bb() 注入；非必需，但补齐更贴近真实客户端）
	req.Header.Set("x-device-brand", "Windows")
	req.Header.Set("x-device-type", "windows")
	req.Header.Set("x-os-version", "10.0.22631")
	req.Header.Set("x-app-version", IdeVersion)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
