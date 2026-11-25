// login — 豆包账号融合后的 TRAE 登录工具。
//
// 流程：生成 PKCE + 设备密钥对 → 打开登录链接（豆包 SSO）→ 粘贴回调链接
// → 用 AuthCode 换 token（v3 ExchangeToken）→ 拉取账号信息 → 保存凭证。
//
// 凭证里会额外保存 devicePrivateKey/devicePublicKey，后续刷新 token
// 需要用它做设备签名（DeviceProof）。
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"trae-signin/internal/auth"
	"trae-signin/internal/upstream"
)

const callbackURL = "http://127.0.0.1:18080/authorize"

func main() {
	var (
		dir       = flag.String("dir", "auths", "凭证保存目录")
		deviceID  = flag.String("device-id", os.Getenv("DEVICE_ID"), "真实设备号（8~24 位数字），留空则自动检测")
		clientID  = flag.String("client-id", upstream.ClientIDTrae, "OAuth client_id（旧 SOLO 账号可用 "+upstream.ClientIDSolo+"）")
		printOnly = flag.Bool("print-url", false, "只打印登录链接，不等待回调")
		noCheckin = flag.Bool("no-checkin", false, "登录后不自动签到")
		noBrowser = flag.Bool("no-browser", false, "不自动打开浏览器")
	)
	flag.Parse()

	did := strings.TrimSpace(*deviceID)
	if did == "" {
		did = detectDeviceID()
		if did != "" {
			fmt.Printf("🔍 已自动检测到设备号: %s\n", did)
		}
	}
	if did == "" {
		fmt.Fprintln(os.Stderr, "❌ 需要真实设备号（否则签到会返回 code=9074）")
		fmt.Fprintln(os.Stderr, "   在装了 TRAE 的机器上找到 storage.json，搜索 iCubeAuthInfo://icube-dc:<数字>")
		fmt.Fprintln(os.Stderr, "   然后运行：DEVICE_ID=<数字> ./login.sh")
		os.Exit(1)
	}
	if !regexp.MustCompile(`^\d{8,24}$`).MatchString(did) {
		fmt.Fprintf(os.Stderr, "❌ 设备号格式错误：%s（应为 8~24 位纯数字）\n", did)
		os.Exit(1)
	}

	privPEM, pubPEM, err := genKeyPair()
	if err != nil {
		fail("生成设备密钥失败: %v", err)
	}
	verifier, challenge := genPKCE()

	loginURL := buildLoginURL(*clientID, did, challenge)
	fmt.Println("============================================================")
	fmt.Println("  TRAE 登录（豆包账号）")
	fmt.Println("============================================================")
	fmt.Println("请在浏览器打开下面的链接，用手机号/验证码完成登录：")
	fmt.Println()
	fmt.Println(loginURL)
	fmt.Println()
	if *printOnly {
		return
	}
	if !*noBrowser {
		_ = openBrowser(loginURL)
	}
	fmt.Println("登录成功后浏览器会跳到打不开的 127.0.0.1 地址，复制地址栏完整链接粘贴到下面。")
	fmt.Print("回调链接: ")

	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		fail("未输入回调链接，已取消")
	}
	authCode, refreshToken, cbUID, cbNick := parseCallback(line)
	if authCode == "" && refreshToken == "" {
		fail("回调链接里没有 authCodeInfo / refreshToken，无法换取 token")
	}

	up := upstream.New()
	a := &auth.Auth{
		DeviceID:         did,
		MachineID:        did,
		Domain:           "trae.cn",
		ApiHost:          upstream.AuthHost,
		ClientID:         *clientID,
		DevicePrivateKey: string(privPEM),
		DevicePublicKey:  string(pubPEM),
	}

	var uid, nickname string
	if authCode != "" {
		fmt.Println("🔄 正在用 AuthCode 换取 token...")
		res, err := up.ExchangeAuthCode(a, authCode, verifier)
		if err != nil {
			fail("换取 token 失败: %v", err)
		}
		a.AccessToken, a.RefreshToken, a.ExpiresAt = res.Token, res.RefreshToken, res.ExpiresAt
	} else {
		fmt.Println("🔄 回调用的是旧格式，正在用 refreshToken 换取 token...")
		a.RefreshToken = refreshToken
		if err := up.RefreshToken(a); err != nil {
			fail("换取 token 失败: %v", err)
		}
	}
	if a.RefreshToken == "" {
		fail("服务端没有返回 refreshToken，无法长期使用")
	}

	uid, nickname, err = up.GetUserInfo(a)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  获取账号信息失败（继续保存）: %v\n", err)
		uid, nickname = cbUID, cbNick
	}
	if uid == "" {
		fail("未能获取 uid，请重试")
	}
	a.UID, a.Nickname = uid, nickname

	_ = os.MkdirAll(*dir, 0o755)
	a.FilePath = filepath.Join(*dir, "trae-"+uid+".json")
	if err := a.SaveAtomic(); err != nil {
		fail("保存凭证失败: %v", err)
	}

	fmt.Println()
	fmt.Printf("✅ 登录完成\n   UID:      %s\n   昵称:     %s\n   有效期至: %s\n   凭证文件: %s\n",
		uid, orDash(nickname), fmtTime(a.ExpiresAt), a.FilePath)

	if *noCheckin {
		return
	}
	// 自动签到 + 查积分（与旧 login.sh 行为一致）
	if checked, _, enable, serr := up.CheckinStatus(a); serr != nil {
		fmt.Printf("签到状态查询失败: %v\n", serr)
	} else if checked {
		fmt.Println("签到: 今日已签到")
	} else if !enable {
		fmt.Println("签到: 当前未开启")
	} else if err := up.CheckinClaim(a); err != nil {
		fmt.Printf("签到失败: %v\n", err)
	} else {
		fmt.Println("签到: 成功")
	}
	if sum, err := up.UserCreditSummary(a); err == nil {
		fmt.Printf("当前积分: %d（月赠 %d / 签到 %d）\n", sum.Total, sum.Month, sum.Checkin)
	}
}

func genKeyPair() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	priv := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	pub := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return priv, pub, nil
}

func genPKCE() (verifier, challenge string) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		fail("生成 PKCE 失败: %v", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func buildLoginURL(clientID, deviceID, challenge string) string {
	trace := make([]byte, 8)
	_, _ = rand.Read(trace)
	v := map[string]string{
		"login_version":         "1",
		"auth_from":             "trae",
		"login_channel":         "native_ide",
		"plugin_version":        "local",
		"auth_type":             "local",
		"client_id":             clientID,
		"redirect":              "0",
		"login_trace_id":        hex.EncodeToString(trace),
		"auth_callback_url":     callbackURL,
		"machine_id":            deviceID,
		"device_id":             deviceID,
		"x_device_id":           deviceID,
		"x_machine_id":          deviceID,
		"x_device_brand":        "PC",
		"x_device_type":         "PC",
		"x_os_version":          "1.0",
		"x_env":                 "prod",
		"x_app_version":         upstream.IdeVersion,
		"x_app_type":            "stable",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"doubao_passport":       "1",
		"loginScope":            "toc",
	}
	return "https://www.trae.cn/authorization?" + encodeQuery(v)
}

func encodeQuery(v map[string]string) string {
	var b strings.Builder
	for k, val := range v {
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(urlEncode(k))
		b.WriteByte('=')
		b.WriteString(urlEncode(val))
	}
	return b.String()
}

func urlEncode(s string) string {
	// 只保留 RFC3986 非保留字符，其余百分号编码（与 Python quote 一致）。
	const hexChars = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexChars[c>>4])
		b.WriteByte(hexChars[c&0x0f])
	}
	return b.String()
}

// parseCallback 从回调 URL 中提取 authCode（新）或 refreshToken（旧）。
func parseCallback(raw string) (authCode, refreshToken, uid, nickname string) {
	q := queryValues(raw)
	for _, key := range []string{"authCodeInfo", "authCode", "code"} {
		v := q[key]
		if v == "" {
			continue
		}
		if obj := parseJSONParam(v); obj != nil {
			for _, k := range []string{"AuthCode", "authCode", "Code"} {
				if s, _ := obj[k].(string); s != "" {
					authCode = s
					break
				}
			}
			if authCode != "" {
				break
			}
			continue
		}
		authCode = v
		break
	}
	refreshToken = q["refreshToken"]
	if ui := parseJSONParam(q["userInfo"]); ui != nil {
		uid = str(ui["UserID"])
		nickname = str(ui["ScreenName"])
	}
	return
}

// queryValues 兼容整段 URL 或仅 query 字符串，并做一次/两次 unquote。
func queryValues(raw string) map[string]string {
	s := raw
	if i := strings.Index(s, "?"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, "#"); i >= 0 {
		s = s[:i]
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		out[unescape(k)] = unescape(v)
	}
	return out
}

func unescape(s string) string {
	if !strings.Contains(s, "%") && !strings.Contains(s, "+") {
		return s
	}
	s = strings.ReplaceAll(s, "+", " ")
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			var v int
			if _, err := fmt.Sscanf(s[i+1:i+3], "%02x", &v); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseJSONParam(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	for i := 0; i < 3 && raw != ""; i++ {
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err == nil {
			return obj
		}
		raw = unescape(raw)
	}
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func detectDeviceID() string {
	var candidates []string
	appData := os.Getenv("APPDATA")
	localApp := os.Getenv("LOCALAPPDATA")
	home, _ := os.UserHomeDir()
	names := []string{"Trae CN", "TRAE SOLO CN", "Trae", "Trae CN Nightly"}
	switch runtime.GOOS {
	case "windows":
		for _, n := range names {
			if appData != "" {
				candidates = append(candidates, filepath.Join(appData, n, "User", "globalStorage", "storage.json"))
			}
			if localApp != "" {
				candidates = append(candidates, filepath.Join(localApp, n, "User", "globalStorage", "storage.json"))
			}
		}
	case "darwin":
		for _, n := range names {
			candidates = append(candidates, filepath.Join(home, "Library", "Application Support", n, "User", "globalStorage", "storage.json"))
		}
	default:
		for _, n := range names {
			candidates = append(candidates, filepath.Join(home, ".config", n, "User", "globalStorage", "storage.json"))
		}
	}
	re := regexp.MustCompile(`iCubeAuthInfo://icube-dc:(\d{8,24})`)
	for _, p := range candidates {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if m := re.FindSubmatch(raw); m != nil {
			return string(m[1])
		}
	}
	return ""
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		return exec.Command("open", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}

func fmtTime(ts int64) string {
	if ts <= 0 {
		return "未知"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "❌ "+format+"\n", args...)
	os.Exit(1)
}
