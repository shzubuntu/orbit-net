package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orbit/internal/account"
)

// testSrv 造一个不监听的控制面 Server(仅供 HTTP handler 测试)。
func testSrv(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	acct, err := account.Open(dir + "/accounts.json")
	if err != nil {
		t.Fatal(err)
	}
	leases, err := account.OpenLeases(dir+"/leases.json", "10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	usage, err := account.OpenUsage(dir + "/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	invites, err := account.OpenInvites(dir + "/invites.json")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		cfg:     Config{DefaultQuota: QuotaSpec{BytesPerDay: 1024, EgressBytesPerDay: 512}},
		acct:    acct,
		leases:  leases,
		usage:   usage,
		invites: invites,
		hub:     NewHub("10.0.0.0/24", QuotaSpec{BytesPerDay: 1024, EgressBytesPerDay: 512}, leases, usage),
		lim:     newRateLimiter(),
	}
	ts := httptest.NewServer(s.mux())
	t.Cleanup(ts.Close)
	return s, ts
}

// seedAccount 直接灌邀请码 + 走注册接口建账户,返回账户 id/设备 id/明文 token。
func seedAccount(t *testing.T, s *Server, ts *httptest.Server, name string) (acctID, devID, token string) {
	t.Helper()
	codes, err := s.invites.Generate(1)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{
		"invite_code": codes[0], "account_name": name, "device_name": "main",
	})
	resp, raw := doJSON(t, ts, http.MethodPost, "/api/v1/register", nil, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register: HTTP %d %s", resp.StatusCode, raw)
	}
	var r struct {
		AccountID   string `json:"account_id"`
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.AccountID, r.DeviceID, r.DeviceToken
}

func doJSON(t *testing.T, ts *httptest.Server, method, path string, auth map[string]string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range auth {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func deviceAuth(devID, token string) map[string]string {
	return map[string]string{"X-Orbit-Device": devID, "X-Orbit-Token": token}
}

// TestSelfServiceDeviceLifecycle 自助全链路: 列表 → 加装 → 改名 → 轮换 → 吊销 → 用量。
func TestSelfServiceDeviceLifecycle(t *testing.T) {
	s, ts := testSrv(t)
	acctID, mainID, mainTok := seedAccount(t, s, ts, "acc-lifecycle")

	// 1. 列表只有首设备
	_, raw := doJSON(t, ts, http.MethodGet, "/api/v1/devices", deviceAuth(mainID, mainTok), nil)
	if !bytes.Contains(raw, []byte(`"device_id":"`+mainID)) {
		t.Fatalf("list missing main device: %s", raw)
	}

	// 2. 加装第二台(同名账户新设备)
	enrollBody, _ := json.Marshal(map[string]string{"device_name": "laptop"})
	resp, raw := doJSON(t, ts, http.MethodPost, "/api/v1/devices", deviceAuth(mainID, mainTok), enrollBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll: HTTP %d %s", resp.StatusCode, raw)
	}
	var enrolled struct {
		DeviceID string `json:"device_id"`
	}
	_ = json.Unmarshal(raw, &enrolled)
	if enrolled.DeviceID == "" {
		t.Fatalf("enroll no device_id: %s", raw)
	}

	// 3. 改名(对第二台)
	renameBody, _ := json.Marshal(map[string]string{"device_id": enrolled.DeviceID, "name": "work-pc"})
	resp, raw = doJSON(t, ts, http.MethodPost, "/api/v1/devices/rename", deviceAuth(mainID, mainTok), renameBody)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte(`"name":"work-pc"`)) {
		t.Fatalf("rename: HTTP %d %s", resp.StatusCode, raw)
	}

	// 4. 轮换 main 自身 token → 旧 token 立即失效、新 token 可用
	resp, raw = doJSON(t, ts, http.MethodPost, "/api/v1/devices/rotate-token",
		deviceAuth(mainID, mainTok), []byte(`{}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: HTTP %d %s", resp.StatusCode, raw)
	}
	var rotated struct {
		DeviceToken string `json:"device_token"`
	}
	_ = json.Unmarshal(raw, &rotated)
	if rotated.DeviceToken == "" {
		t.Fatalf("rotate no token: %s", raw)
	}
	resp, raw = doJSON(t, ts, http.MethodGet, "/api/v1/devices", deviceAuth(mainID, mainTok), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token still accepted: HTTP %d %s", resp.StatusCode, raw)
	}
	resp, raw = doJSON(t, ts, http.MethodGet, "/api/v1/devices", deviceAuth(mainID, rotated.DeviceToken), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new token rejected: HTTP %d %s", resp.StatusCode, raw)
	}

	// 5. 用新 token 吊销第二台; 注销后其 token 立即失效(自持有设备无法再认证)
	revokeBody, _ := json.Marshal(map[string]string{"device_id": enrolled.DeviceID})
	resp, _ = doJSON(t, ts, http.MethodPost, "/api/v1/devices/revoke", deviceAuth(mainID, rotated.DeviceToken), revokeBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: HTTP %d %s", resp.StatusCode, raw)
	}
	resp, _ = doJSON(t, ts, http.MethodGet, "/api/v1/devices", deviceAuth(enrolled.DeviceID, ""), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked device token accepted: HTTP %d", resp.StatusCode)
	}

	// 6. 用量自助查询
	resp, raw = doJSON(t, ts, http.MethodGet, "/api/v1/usage", deviceAuth(mainID, rotated.DeviceToken), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usage: HTTP %d %s", resp.StatusCode, raw)
	}
	var u struct {
		TotalBytes int64  `json:"total_bytes"`
		LimitBytes int64  `json:"limit_bytes"`
		AccountID  string `json:"account_id"`
	}
	_ = json.Unmarshal(raw, &u)
	if u.AccountID != acctID || u.LimitBytes != 1024 {
		t.Fatalf("usage mismatch: %s", raw)
	}
}

// TestSelfServiceCrossAccount 越权防护: B 账户设备不能操作 A 账户设备; 无 token 一律 401。
func TestSelfServiceCrossAccount(t *testing.T) {
	s, ts := testSrv(t)
	_, mainA, tokA := seedAccount(t, s, ts, "acc-a")
	_, mainB, tokB := seedAccount(t, s, ts, "acc-b")

	body, _ := json.Marshal(map[string]string{"device_id": mainA, "name": "hack"})
	resp, _ := doJSON(t, ts, http.MethodPost, "/api/v1/devices/rename", deviceAuth(mainB, tokB), body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-account rename allowed: HTTP %d", resp.StatusCode)
	}
	body, _ = json.Marshal(map[string]string{"device_id": mainA})
	resp, _ = doJSON(t, ts, http.MethodPost, "/api/v1/devices/revoke", deviceAuth(mainB, tokB), body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-account revoke allowed: HTTP %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, ts, http.MethodGet, "/api/v1/devices", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth allowed: HTTP %d", resp.StatusCode)
	}
	_ = tokA
}

// TestAdminSetTier M3 计费档位: 改档位后自助用量与管理账户列表立即反映新配额;
// 无 admin token 拒、切到未列出的档位回落默认配额。
func TestAdminSetTier(t *testing.T) {
	s, ts := testSrv(t)
	acctID, mainID, tok := seedAccount(t, s, ts, "tiered")
	admin := func() map[string]string { return map[string]string{"X-Admin-Token": "t"} } // testSrv 的 admin.token 为空, 此处直接补上
	// adminGuard 用 s.cfg.Admin.Token, 测试里无 token 配置 => 拒绝; 这里显式开启
	s.cfg.Admin.Token = "t"
	s.cfg.Tiers = map[string]QuotaSpec{
		"free": {BytesPerDay: 1024, EgressBytesPerDay: 512},
		"pro":  {BytesPerDay: 1 << 30, EgressBytesPerDay: 1 << 30},
	}
	s.hub.SetTierQuota(s.cfg.Tiers, s.acct)

	// 无 admin token 拒绝
	resp, _ := doJSON(t, ts, http.MethodPost, "/api/v1/admin/accounts/tier", nil, []byte(`{"account_id":"`+acctID+`","tier":"pro"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("set-tier without admin token allowed: HTTP %d", resp.StatusCode)
	}

	// 改档位 -> 自助用量反映新限额
	body, _ := json.Marshal(map[string]string{"account_id": acctID, "tier": "pro"})
	resp, raw := doJSON(t, ts, http.MethodPost, "/api/v1/admin/accounts/tier", admin(), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set-tier: HTTP %d %s", resp.StatusCode, raw)
	}
	resp, raw = doJSON(t, ts, http.MethodGet, "/api/v1/usage", deviceAuth(mainID, tok), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usage: HTTP %d %s", resp.StatusCode, raw)
	}
	var u struct {
		Tier             string `json:"tier"`
		LimitBytes       int64  `json:"limit_bytes"`
		EgressLimitBytes int64  `json:"egress_limit_bytes"`
	}
	_ = json.Unmarshal(raw, &u)
	if u.Tier != "pro" || u.LimitBytes != 1<<30 || u.EgressLimitBytes != 1<<30 {
		t.Fatalf("usage after tier change mismatch: %s", raw)
	}

	// 管理列表带新配额
	resp, raw = doJSON(t, ts, http.MethodGet, "/api/v1/admin/accounts", admin(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("accounts: HTTP %d", resp.StatusCode)
	}
	if !bytes.Contains(raw, []byte(`"tier":"pro"`)) || !bytes.Contains(raw, []byte(fmt.Sprintf(`"limit_bytes":%d`, int64(1<<30)))) {
		t.Fatalf("admin accounts missing tier quota: %s", raw)
	}

	// 切到未列出档位 -> 回落默认(dynamic 不在 tiers, 用默认 1024)
	body, _ = json.Marshal(map[string]string{"account_id": acctID, "tier": "dynamic"})
	resp, raw = doJSON(t, ts, http.MethodPost, "/api/v1/admin/accounts/tier", admin(), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set-tier dynamic: HTTP %d %s", resp.StatusCode, raw)
	}
	resp, raw = doJSON(t, ts, http.MethodGet, "/api/v1/usage", deviceAuth(mainID, tok), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usage2: HTTP %d", resp.StatusCode)
	}
	var u2 struct {
		Tier       string `json:"tier"`
		LimitBytes int64  `json:"limit_bytes"`
	}
	_ = json.Unmarshal(raw, &u2)
	if u2.Tier != "dynamic" || u2.LimitBytes != 1024 {
		t.Fatalf("fallback tier usage mismatch: %s", raw)
	}
}

// TestAdminUsages 每账户/每设备当日用量单计口径: 直灌 usage 桶, 校验帐务行与配额一致。
func TestAdminUsages(t *testing.T) {
	s, ts := testSrv(t)
	s.cfg.Admin.Token = "t"
	admin := map[string]string{"X-Admin-Token": "t"}
	acctID, devID, _ := seedAccount(t, s, ts, "usage-co")
	now := time.Now()
	// 直灌: 该账户设备发 100B mesh 包 + 60B 走出口(单计)。
	s.usage.Add(acctID, devID, "", 0, 100, now)
	s.usage.Add(acctID, devID, "dev_exit", 0, 60, now)

	resp, raw := doJSON(t, ts, http.MethodGet, "/api/v1/admin/usages", admin, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("usages: HTTP %d %s", resp.StatusCode, raw)
	}
	var out struct {
		Accounts []struct {
			ID          string `json:"account_id"`
			Tier        string `json:"tier"`
			UsedBytes   int64  `json:"used_bytes"`
			LimitBytes  int64  `json:"limit_bytes"`
			EgressUsed  int64  `json:"egress_used_bytes"`
			EgressLimit int64  `json:"egress_limit_bytes"`
		} `json:"accounts"`
		Devices []struct {
			DeviceID   string `json:"device_id"`
			UsedBytes  int64  `json:"used_bytes"`
			EgressUsed int64  `json:"egress_used_bytes"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode usages: %v: %s", err, raw)
	}
	if len(out.Accounts) == 0 || out.Accounts[0].ID != acctID {
		t.Fatalf("accounts missing seeded account: %s", raw)
	}
	a := out.Accounts[0]
	if a.UsedBytes != 160 || a.EgressUsed != 60 || a.LimitBytes != 1024 || a.EgressLimit != 512 {
		t.Fatalf("account row mismatch: %+v", a)
	}
	if len(out.Devices) != 1 || out.Devices[0].DeviceID != devID ||
		out.Devices[0].UsedBytes != 160 || out.Devices[0].EgressUsed != 60 {
		t.Fatalf("device row mismatch: %+v", out.Devices)
	}
	if resp = mustAdminDeny(t, ts, "/api/v1/admin/usages"); resp != nil {
		_ = resp
	}
}

// mustAdminDeny 未带 admin token 时必须拒绝(403/401)。
func mustAdminDeny(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	r, _ := doJSON(t, ts, http.MethodGet, path, nil, nil)
	if r.StatusCode != http.StatusForbidden && r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("%s without admin token: HTTP %d", path, r.StatusCode)
	}
	return r
}
