package server

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	"orbit/internal/account"
	"orbit/internal/version"
)

// Serve 管理 HTTP(both public 注册入口与 admin 受保护端点)。
// main 里以 goroutine 方式启动,与数据面并行。
func (s *Server) ListenAndServe() error {
	addr := s.cfg.Admin.Addr
	if addr == "" {
		return errServerClosed
	}
	srv := &http.Server{Addr: addr, Handler: s.mux()}
	return srv.ListenAndServe()
}

func (s *Server) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/health", s.handleHealth)
	// 公网入口(邀请码注册 / 设备自助管理: 列表/加装/改名/吊销/轮换 token/用量)
	m.HandleFunc("/api/v1/register", s.handleRegister)
	m.HandleFunc("/api/v1/devices", s.handleDevices)
	m.HandleFunc("/api/v1/devices/rename", s.handleDeviceRename)
	m.HandleFunc("/api/v1/devices/revoke", s.handleDeviceRevoke)
	m.HandleFunc("/api/v1/devices/rotate-token", s.handleDeviceRotateToken)
	m.HandleFunc("/api/v1/usage", s.handleSelfUsage)
	// 管理面(Admin-Token)
	m.HandleFunc("/api/v1/admin/accounts", s.adminGuard(s.handleAdminAccounts))
	m.HandleFunc("/api/v1/admin/accounts/tier", s.adminGuard(s.handleAdminSetTier))
	m.HandleFunc("/api/v1/admin/clients", s.adminGuard(s.handleAdminClients))
	m.HandleFunc("/api/v1/admin/usage", s.adminGuard(s.handleAdminUsage))
	m.HandleFunc("/api/v1/admin/usages", s.adminGuard(s.handleAdminUsages))
	m.HandleFunc("/api/v1/admin/invites", s.adminGuard(s.handleAdminInvites))
	m.HandleFunc("/api/v1/admin/devices", s.adminGuard(s.handleAdminDevices))
	m.HandleFunc("/api/v1/admin/devices/add", s.adminGuard(s.handleAdminAddDevice))
	m.HandleFunc("/api/v1/admin/devices/revoke", s.adminGuard(s.handleAdminRevokeDevice))
	m.HandleFunc("/api/v1/admin/invites/remove", s.adminGuard(s.handleAdminRemoveInvite))
	m.HandleFunc("/api/v1/admin/egress", s.adminGuard(s.handleAdminEgress))
	// 内嵌管理 UI(根路径; 未知路径 404)
	m.HandleFunc("/", s.handleWebUI)
	return m
}

// ==================== 公网入口 ====================

type regReq struct {
	InviteCode  string `json:"invite_code"`
	AccountName string `json:"account_name"`
	DeviceName  string `json:"device_name,omitempty"`
}

// handleRegister 邀请码注册: 建账户 + 首设备 + 落租约网络,返回一次性 token。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.lim.Allow("register:"+remoteIP(r), 5, time.Minute) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	var req regReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.InviteCode == "" || req.AccountName == "" {
		http.Error(w, "invite_code and account_name required", http.StatusBadRequest)
		return
	}
	if len(req.AccountName) > 64 {
		http.Error(w, "account_name too long", http.StatusBadRequest)
		return
	}
	acct, dev, token, err := s.acct.CreateAccountWithTier(req.AccountName, s.defaultTier())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.invites.Consume(req.InviteCode, acct.ID); err != nil {
		_ = s.acct.RemoveAccount(acct.ID)
		http.Error(w, "invite: "+err.Error(), http.StatusForbidden)
		return
	}
	// 可在同一请求里加装第二台设备?不需要,首设备即 main。
	writeJSON(w, map[string]any{
		"ok":           true,
		"account_id":   acct.ID,
		"account_name": acct.Name,
		"tier":         acct.Tier,
		"device_id":    dev.ID,
		"device_token": token, // 一次性明文
	})
}

type enrollReq struct {
	DeviceName string `json:"device_name"`
}

// handleDevices 设备自助管理入口: GET=列本账户设备, POST=用一台已有设备 token 加装新设备。
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.deviceList(w, r)
	case http.MethodPost:
		s.handleEnrollDevice(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// deviceAuth 设备-token 鉴权(自助管理用): 返回设备与其所属账户。
func (s *Server) deviceAuth(r *http.Request) (*account.Device, *account.Account, error) {
	return s.acct.VerifyDevice(r.Header.Get("X-Orbit-Device"), r.Header.Get("X-Orbit-Token"))
}

// deviceList 本账户设备列表(自助): 名字/出口开关/在线状态/最近在线。
func (s *Server) deviceList(w http.ResponseWriter, r *http.Request) {
	_, acct, err := s.deviceAuth(r)
	if err != nil {
		http.Error(w, "device auth failed", http.StatusUnauthorized)
		return
	}
	online := make(map[string]bool)
	for _, c := range s.hub.ListClients() {
		online[c.DeviceID] = true
	}
	type item struct {
		DeviceID      string   `json:"device_id"`
		Name          string   `json:"name"`
		EgressEnabled bool     `json:"egress_enabled"`
		EgressACL     []string `json:"egress_acl"`
		LastSeen      string   `json:"last_seen"`
		Online        bool     `json:"online"`
	}
	var out []item
	for _, d := range s.acct.DevicesByAccount(acct.ID) {
		out = append(out, item{
			DeviceID: d.ID, Name: d.Name,
			EgressEnabled: d.EgressEnabled, EgressACL: d.EgressACL,
			LastSeen: d.LastSeen.Format(time.RFC3339), Online: online[d.ID],
		})
	}
	writeJSON(w, out)
}

// handleEnrollDevice 用一台已有设备的 token 给同一账户加装新设备。
// 鉴权: X-Orbit-Device / X-Orbit-Token 请求头。
func (s *Server) handleEnrollDevice(w http.ResponseWriter, r *http.Request) {
	if !s.lim.Allow("enroll:"+remoteIP(r), 30, time.Hour) {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	dev, acct, err := s.deviceAuth(r)
	if err != nil {
		http.Error(w, "device auth failed", http.StatusUnauthorized)
		return
	}
	var req enrollReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	name := req.DeviceName
	if name == "" {
		name = dev.Name + "-new"
	}
	newDev, newToken, err := s.acct.AddDeviceForAccount(acct.ID, name)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("device %s self-enrolled %s (%s)", dev.ID, newDev.ID, newDev.Name)
	writeJSON(w, map[string]any{
		"ok": true, "account_id": acct.ID,
		"device_id": newDev.ID, "device_token": newToken,
	})
}

// targetDevice 解析操作目标设备: 默认=调用者自己; 指定时必须是同账户设备(不允许越权)。
func (s *Server) targetDevice(r *http.Request, reqd *string) (*account.Device, error) {
	dev, acct, err := s.deviceAuth(r)
	if err != nil {
		return nil, err
	}
	id := ""
	if reqd != nil {
		id = *reqd
	}
	if id == "" {
		return dev, nil
	}
	d, err := s.acct.Device(id)
	if err != nil {
		return nil, err
	}
	if d.AccountID != acct.ID {
		return nil, account.ErrBadToken
	}
	return d, nil
}

// handleDeviceRename 自助改名(对自己或同账户设备)。
func (s *Server) handleDeviceRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		DeviceID string `json:"device_id"`
		Name     string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	target, err := s.targetDevice(r, &req.DeviceID)
	if err != nil {
		http.Error(w, "device auth failed", http.StatusUnauthorized)
		return
	}
	updated, err := s.acct.RenameDevice(target.ID, req.Name)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("device %s self-renamed to %s", target.ID, updated.Name)
	writeJSON(w, map[string]any{"ok": true, "device_id": updated.ID, "name": updated.Name})
}

// handleDeviceRevoke 自助吊销设备(同账户; 包含自己)。权限即刻失效并踢下线。
func (s *Server) handleDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceID == "" {
		http.Error(w, "device_id required", http.StatusBadRequest)
		return
	}
	target, err := s.targetDevice(r, &req.DeviceID)
	if err != nil {
		http.Error(w, "device auth failed", http.StatusUnauthorized)
		return
	}
	if err := s.acct.RemoveDevice(target.ID); err != nil {
		s.fail(w, http.StatusNotFound, err.Error())
		return
	}
	kicked := s.hub.KickDevice(target.ID)
	log.Printf("device %s self-revoked (kicked=%v)", target.ID, kicked)
	writeJSON(w, map[string]any{"ok": true, "revoked": target.ID, "kicked": kicked})
}

// handleDeviceRotateToken 自助轮换设备 token(自己或同账户设备)。旧 token 即刻失效。
func (s *Server) handleDeviceRotateToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	target, err := s.targetDevice(r, &req.DeviceID)
	if err != nil {
		http.Error(w, "device auth failed", http.StatusUnauthorized)
		return
	}
	token, err := s.acct.RotateToken(target.ID)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("device %s rotated its token", target.ID)
	writeJSON(w, map[string]any{"ok": true, "device_id": target.ID, "device_token": token})
}

// handleSelfUsage 自助用量/配额查询(本账户当日)。
func (s *Server) handleSelfUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	_, acct, err := s.deviceAuth(r)
	if err != nil {
		http.Error(w, "device auth failed", http.StatusUnauthorized)
		return
	}
	since := time.Now().Truncate(24 * time.Hour)
	t := s.usage.Totals(acct.ID, since)
	spec := s.cfg.QuotaFor(acct.Tier)
	writeJSON(w, map[string]any{
		"period": "day", "since": since.Format(time.RFC3339),
		"account_id": acct.ID, "account_name": acct.Name, "tier": acct.Tier,
		"rx_bytes": t.RxBytes, "tx_bytes": t.TxBytes, "total_bytes": t.RxBytes + t.TxBytes,
		"limit_bytes":        spec.BytesPerDay,
		"egress_limit_bytes": spec.EgressBytesPerDay,
	})
}

// ==================== 管理面 ====================

func (s *Server) handleAdminAccounts(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Tier             string `json:"tier"`
		Devices          int    `json:"devices"`
		LimitBytes       int64  `json:"limit_bytes"`
		EgressLimitBytes int64  `json:"egress_limit_bytes"`
	}
	var out []item
	for _, a := range s.acct.ListAccounts() {
		spec := s.cfg.QuotaFor(a.Tier)
		out = append(out, item{
			ID: a.ID, Name: a.Name, Tier: a.Tier, Devices: len(s.acct.DevicesByAccount(a.ID)),
			LimitBytes: spec.BytesPerDay, EgressLimitBytes: spec.EgressBytesPerDay,
		})
	}
	writeJSON(w, out)
}

// handleAdminSetTier 修改账户档位(M3 计费档位): 落库后向该账户在线节点推送新配额。
func (s *Server) handleAdminSetTier(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
		Tier      string `json:"tier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccountID == "" || req.Tier == "" {
		http.Error(w, "account_id and tier required", http.StatusBadRequest)
		return
	}
	if err := s.acct.SetAccountTier(req.AccountID, req.Tier); err != nil {
		s.fail(w, http.StatusNotFound, err.Error())
		return
	}
	s.hub.QuotaNotifyAccount(req.AccountID)
	log.Printf("admin: set account %s tier=%s", req.AccountID, req.Tier)
	writeJSON(w, map[string]any{"ok": true, "account_id": req.AccountID, "tier": req.Tier})
}

func (s *Server) handleAdminClients(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.hub.ListClients())
}

func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Truncate(24 * time.Hour)
	accountID := r.URL.Query().Get("account")
	resp := map[string]any{"period": "day", "since": since.Format(time.RFC3339)}
	if accountID != "" {
		t := s.usage.Totals(accountID, since)
		resp["account"] = accountID
		resp["rx_bytes"] = t.RxBytes
		resp["tx_bytes"] = t.TxBytes
		resp["total_bytes"] = t.RxBytes + t.TxBytes
	} else {
		// 全部账户汇总
		totRx, totTx := int64(0), int64(0)
		for _, a := range s.acct.ListAccounts() {
			t := s.usage.Totals(a.ID, since)
			totRx += t.RxBytes
			totTx += t.TxBytes
		}
		resp["rx_bytes"] = totRx
		resp["tx_bytes"] = totTx
		resp["total_bytes"] = totRx + totTx
	}
	writeJSON(w, resp)
}

// handleAdminUsages 每账户 + 每消费者设备当日用量(单计口径, 与配额实时一致)。
func (s *Server) handleAdminUsages(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Truncate(24 * time.Hour)
	rows := s.usage.PerConsumer(since)

	nameOf := map[string]string{}
	for _, d := range s.acct.ListDevicesAll() {
		nameOf[d.ID] = d.Name
	}

	type devRow struct {
		DeviceID    string `json:"device_id"`
		AccountID   string `json:"account_id"`
		Name        string `json:"name"`
		UsedBytes   int64  `json:"used_bytes"`
		EgressBytes int64  `json:"egress_used_bytes"`
	}
	acctUsed := map[string][2]int64{} // account -> [tx, egress]
	devices := []devRow{}
	for _, row := range rows {
		a := acctUsed[row.AccountID]
		a[0] += row.TxBytes
		a[1] += row.EgressBytes
		acctUsed[row.AccountID] = a
		devices = append(devices, devRow{
			DeviceID: row.DeviceID, AccountID: row.AccountID,
			Name: nameOf[row.DeviceID], UsedBytes: row.TxBytes, EgressBytes: row.EgressBytes,
		})
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].AccountID != devices[j].AccountID {
			return devices[i].AccountID < devices[j].AccountID
		}
		return devices[i].DeviceID < devices[j].DeviceID
	})

	type acctRow struct {
		AccountID   string `json:"account_id"`
		Name        string `json:"name"`
		Tier        string `json:"tier"`
		UsedBytes   int64  `json:"used_bytes"`
		LimitBytes  int64  `json:"limit_bytes"`
		EgressUsed  int64  `json:"egress_used_bytes"`
		EgressLimit int64  `json:"egress_limit_bytes"`
	}
	accounts := []acctRow{}
	for _, a := range s.acct.ListAccounts() {
		spec := s.cfg.QuotaFor(a.Tier)
		u := acctUsed[a.ID]
		accounts = append(accounts, acctRow{
			AccountID: a.ID, Name: a.Name, Tier: a.Tier,
			UsedBytes: u[0], LimitBytes: spec.BytesPerDay,
			EgressUsed: u[1], EgressLimit: spec.EgressBytesPerDay,
		})
	}
	writeJSON(w, map[string]any{
		"period": "day", "since": since.Format(time.RFC3339),
		"accounts": accounts, "devices": devices,
	})
}

func (s *Server) handleAdminInvites(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// 生成 n(默认 10)个邀请码
		n := 10
		if v := r.URL.Query().Get("count"); v != "" {
			if x, err := strconv.Atoi(v); err == nil && x > 0 && x <= 1000 {
				n = x
			}
		}
		codes, err := s.invites.Generate(n)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "invites": codes, "count": n})
		return
	}
	writeJSON(w, s.invites.List())
}

// ==================== 公共工具 ====================

func (s *Server) handleAdminDevices(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		ID            string   `json:"device_id"`
		AccountID     string   `json:"account_id"`
		Name          string   `json:"name"`
		EgressEnabled bool     `json:"egress_enabled"`
		EgressACL     []string `json:"egress_acl"`
		LastSeen      string   `json:"last_seen"`
		Online        bool     `json:"online"`
	}
	online := make(map[string]bool)
	for _, c := range s.hub.ListClients() {
		online[c.DeviceID] = true
	}
	var out []item
	for _, d := range s.acct.ListDevicesAll() {
		out = append(out, item{
			ID: d.ID, AccountID: d.AccountID, Name: d.Name,
			EgressEnabled: d.EgressEnabled, EgressACL: d.EgressACL,
			LastSeen: d.LastSeen.Format(time.RFC3339), Online: online[d.ID],
		})
	}
	writeJSON(w, out)
}

// handleAdminRevokeDevice 撤销设备: 删除记录 + 踢下线(权限即刻失效)。
func (s *Server) handleAdminRevokeDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceID == "" {
		http.Error(w, "device_id required", http.StatusBadRequest)
		return
	}
	if err := s.acct.RemoveDevice(req.DeviceID); err != nil {
		s.fail(w, http.StatusNotFound, err.Error())
		return
	}
	kicked := s.hub.KickDevice(req.DeviceID)
	log.Printf("admin: revoked device %s (kicked=%v)", req.DeviceID, kicked)
	writeJSON(w, map[string]any{"ok": true, "revoked": req.DeviceID, "kicked": kicked})
}

// handleAdminAddDevice 管理面为指定账户签发新设备, 返回一次性明文 token。
func (s *Server) handleAdminAddDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AccountID string `json:"account_id"`
		Name      string `json:"name,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccountID == "" {
		http.Error(w, "account_id required", http.StatusBadRequest)
		return
	}
	dev, token, err := s.acct.AddDeviceForAccount(req.AccountID, req.Name)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("admin: added device %s (%s) to account %s", dev.ID, dev.Name, req.AccountID)
	writeJSON(w, map[string]any{
		"ok": true, "account_id": req.AccountID,
		"device_id": dev.ID, "device_token": token, // 一次性明文
	})
}

// handleAdminRemoveInvite 吊销未消费邀请码。
func (s *Server) handleAdminRemoveInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
		http.Error(w, "code required", http.StatusBadRequest)
		return
	}
	if err := s.invites.Remove(req.Code); err != nil {
		s.fail(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("admin: revoked invite %s", req.Code)
	writeJSON(w, map[string]any{"ok": true, "revoked": req.Code})
}

// handleAdminEgress 设置设备的出口授权(enabled + ACL), 热更新在线节点。
func (s *Server) handleAdminEgress(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID string   `json:"device_id"`
		Enabled  bool     `json:"enabled"`
		Accounts []string `json:"accounts"` // 允许消费本出口的账户 id 列表; 空=仅本人账户
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceID == "" {
		http.Error(w, "device_id required", http.StatusBadRequest)
		return
	}
	if err := s.acct.SetDeviceEgress(req.DeviceID, req.Enabled, req.Accounts); err != nil {
		s.fail(w, http.StatusNotFound, err.Error())
		return
	}
	live := s.hub.UpdateEgress(req.DeviceID, req.Enabled, req.Accounts)
	log.Printf("admin: set egress %s enabled=%v acl=%v (live=%v)", req.DeviceID, req.Enabled, req.Accounts, live)
	writeJSON(w, map[string]any{"ok": true, "device_id": req.DeviceID, "enabled": req.Enabled, "accounts": req.Accounts})
}

// adminGuard 校验 X-Admin-Token(常数时间比较)。
func (s *Server) adminGuard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Admin.Token == "" {
			http.Error(w, "admin disabled", http.StatusForbidden)
			return
		}
		got := r.Header.Get("X-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Admin.Token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// defaultTier 新账户默认档位(配置未设置时回落 free)。
func (s *Server) defaultTier() string {
	if s.cfg.DefaultTier == "" {
		return "free"
	}
	return s.cfg.DefaultTier
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"ok":      true,
		"version": version.Version,
		"time":    time.Now().Format(time.RFC3339),
	})
}

func (s *Server) fail(w http.ResponseWriter, code int, msg string) {
	http.Error(w, msg, code)
}

func remoteIP(r *http.Request) string {
	// 单机部署,取直连 IP;nginx 前置时代改取 X-Forwarded-For 首项
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
