// Copyright 2019 The Gaea Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"crypto/md5"
	errs "errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/XiaoMi/Gaea/log/zap"

	"github.com/XiaoMi/Gaea/backend"
	"go.uber.org/atomic"

	"github.com/XiaoMi/Gaea/core/errors"
	"github.com/XiaoMi/Gaea/log"
	"github.com/XiaoMi/Gaea/models"
	"github.com/XiaoMi/Gaea/mysql"
	"github.com/XiaoMi/Gaea/parser"
	"github.com/XiaoMi/Gaea/stats"
	"github.com/XiaoMi/Gaea/stats/prometheus"
	"github.com/XiaoMi/Gaea/util"
	"github.com/XiaoMi/Gaea/util/sync2"
	"github.com/shirou/gopsutil/process"
)

const (
	MasterRole               = "master"
	SlaveRole                = "slave"
	StatisticSlaveRole       = "statistic-slave"
	SQLExecTimeSize          = 5000
	DefaultDatacenter        = "default"
	SQLExecStatusOk          = "OK"
	SQLExecStatusErr         = "ERROR"
	SQLExecStatusIgnore      = "IGNORE"
	SQLExecStatusSlow        = "SLOW"
	SQLBackendExecStatusSlow = "backend SLOW"
	SQLBackendExecStatusErr  = "backend ERR"
)

// LoadAndCreateManager load namespace config, and create manager
func LoadAndCreateManager(cfg *models.Proxy) (*Manager, error) {
	namespaceConfigs, err := loadAllNamespace(cfg)
	if err != nil {
		log.Warn("init namespace manager failed, %v", err)
		return nil, err

	}

	mgr, err := CreateManager(cfg, namespaceConfigs)
	if err != nil {
		log.Warn("create manager error: %v", err)
		return nil, err
	}
	//globalManager = mgr
	return mgr, nil
}

func loadAllNamespace(cfg *models.Proxy) (map[string]*models.Namespace, error) {
	// get names of all namespace
	root := cfg.CoordinatorRoot
	if cfg.ConfigType == models.ConfigFile {
		root = cfg.FileConfigPath
	}

	client := models.NewClient(cfg.ConfigType, cfg.CoordinatorAddr, cfg.UserName, cfg.Password, root)
	if client == nil {
		return map[string]*models.Namespace{}, fmt.Errorf("client is nil")
	}
	store := models.NewStore(client)
	defer store.Close()
	var err error
	var names []string
	names, err = store.ListNamespace()
	if err != nil {
		log.Warn("list namespace failed, err: %v", err)
		return nil, err
	}

	// query remote namespace models in worker goroutines
	nameC := make(chan string)
	namespaceC := make(chan *models.Namespace)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			client := models.NewClient(cfg.ConfigType, cfg.CoordinatorAddr, cfg.UserName, cfg.Password, root)
			store := models.NewStore(client)
			defer store.Close()
			defer wg.Done()
			for name := range nameC {
				namespace, e := store.LoadNamespace(cfg.EncryptKey, name)
				if e != nil {
					log.Warn("load namespace %s failed, err: %v", name, err)
					// assign extent err out of this scope
					err = e
					return
				}

				namespaceC <- namespace
			}
		}()
	}

	// dispatch goroutine
	go func() {
		for _, name := range names {
			nameC <- name
		}
		close(nameC)
		wg.Wait()
		close(namespaceC)
	}()

	// collect all namespaces
	namespaceModels := make(map[string]*models.Namespace, 64)
	for namespace := range namespaceC {
		namespaceModels[namespace.Name] = namespace
	}
	if err != nil {
		log.Warn("get namespace failed, err:%v", err)
		return nil, err
	}

	return namespaceModels, nil
}

// Manager contains namespace manager and user manager
type Manager struct {
	reloadPrepared sync2.AtomicBool
	switchIndex    util.BoolIndex
	namespaces     [2]*NamespaceManager
	users          [2]*UserManager
	statistics     *StatisticManager
}

// NewManager return empty Manager
func NewManager() *Manager {
	return &Manager{}
}

// CreateManager create manager
func CreateManager(cfg *models.Proxy, namespaceConfigs map[string]*models.Namespace) (*Manager, error) {
	m := NewManager()

	// init statistics
	statisticManager, err := CreateStatisticManager(cfg, m)
	if err != nil {
		log.Warn("init stats manager failed, %v", err)
		return nil, err
	}
	m.statistics = statisticManager

	current, _, _ := m.switchIndex.Get()

	// init namespace
	m.namespaces[current] = CreateNamespaceManager(namespaceConfigs)

	// init user
	user, err := CreateUserManager(namespaceConfigs)
	if err != nil {
		return nil, err
	}
	m.users[current] = user

	m.startConnectPoolMetricsTask(cfg.StatsInterval)
	return m, nil
}

// Close close manager
func (m *Manager) Close() {
	current, _, _ := m.switchIndex.Get()

	namespaces := m.namespaces[current].namespaces
	for _, ns := range namespaces {
		ns.Close(false)
	}

	m.statistics.Close()
	if m.statistics.generalLogger != nil {
		// 日志落盘
		m.statistics.generalLogger.Close()
	}
}

// ReloadNamespacePrepare prepare commit
func (m *Manager) ReloadNamespacePrepare(namespaceConfig *models.Namespace) error {
	name := namespaceConfig.Name
	current, other, _ := m.switchIndex.Get()
	// reload namespace prepare
	currentNamespaceManager := m.namespaces[current]

	nsOld := currentNamespaceManager.GetNamespace(name)
	var nsChangeIndexOld uint32
	if nsOld != nil {
		nsChangeIndexOld = nsOld.namespaceChangeIndex
	}

	newNamespaceManager := ShallowCopyNamespaceManager(currentNamespaceManager)
	if err := newNamespaceManager.RebuildNamespace(namespaceConfig); err != nil {
		log.Warn("prepare config of namespace: %s failed, err: %v", name, err)
		return err
	}

	newNamespaceManager.GetNamespace(name).namespaceChangeIndex = nsChangeIndexOld + 1

	m.namespaces[other] = newNamespaceManager

	// reload user prepare
	currentUserManager := m.users[current]
	newUserManager := CloneUserManager(currentUserManager)
	newUserManager.RebuildNamespaceUsers(namespaceConfig)
	m.users[other] = newUserManager
	if _, ok := m.statistics.SQLResponsePercentile[name]; !ok {
		m.statistics.SQLResponsePercentile[name] = NewSQLResponse(name)
	}
	m.reloadPrepared.Set(true)

	return nil
}

// ReloadNamespaceCommit commit config
func (m *Manager) ReloadNamespaceCommit(name string) error {
	if !m.reloadPrepared.CompareAndSwap(true, false) {
		err := errors.ErrNamespaceNotPrepared
		log.Warn("commit namespace error, namespace: %s, err: %v", name, err)
		return err
	}

	current, _, index := m.switchIndex.Get()

	currentNamespace := m.namespaces[current].GetNamespace(name)
	if currentNamespace != nil {
		go currentNamespace.Close(true)
	}

	m.switchIndex.Set(!index)

	return nil
}

// DeleteNamespace delete namespace
func (m *Manager) DeleteNamespace(name string) error {
	current, other, index := m.switchIndex.Get()

	// idempotent delete
	currentNamespace := m.namespaces[current].GetNamespace(name)
	if currentNamespace == nil {
		return nil
	}

	// delete namespace of other
	currentNamespaceManager := m.namespaces[current]
	newNamespaceManager := ShallowCopyNamespaceManager(currentNamespaceManager)
	newNamespaceManager.DeleteNamespace(name)
	m.namespaces[other] = newNamespaceManager

	// delete users of other
	currentUserManager := m.users[current]
	newUserManager := CloneUserManager(currentUserManager)
	newUserManager.ClearNamespaceUsers(name)
	m.users[other] = newUserManager

	// switch namespace manager
	m.switchIndex.Set(!index)

	// delay recycle resources of current
	go currentNamespace.Close(true)

	return nil
}

// GetNamespace return specific namespace
func (m *Manager) GetNamespace(name string) *Namespace {
	current, _, _ := m.switchIndex.Get()
	return m.namespaces[current].GetNamespace(name)
}

// CheckUser check if user in users
func (m *Manager) CheckUser(user string) bool {
	current, _, _ := m.switchIndex.Get()
	return m.users[current].CheckUser(user)
}

// CheckPassword check if right password with specific user
func (m *Manager) CheckPassword(user string, salt, auth []byte) (bool, string) {
	current, _, _ := m.switchIndex.Get()
	return m.users[current].CheckPassword(user, salt, auth)
}

// CheckHashPassword check if right password with specific user
func (m *Manager) CheckHashPassword(user string, salt, auth []byte) (bool, string) {
	current, _, _ := m.switchIndex.Get()
	return m.users[current].CheckHashPassword(user, salt, auth)
}

// CheckPassword check if right password with specific user
func (m *Manager) CheckSha2Password(user string, salt, auth []byte) (bool, string) {
	current, _, _ := m.switchIndex.Get()
	return m.users[current].CheckSha2Password(user, salt, auth)
}

// GetStatisticManager return proxy status to record status
func (m *Manager) GetStatisticManager() *StatisticManager {
	return m.statistics
}

// GetNamespaceByUser return namespace by user
func (m *Manager) GetNamespaceByUser(userName, password string) string {
	current, _, _ := m.switchIndex.Get()
	return m.users[current].GetNamespaceByUser(userName, password)
}

// ConfigFingerprint return config fingerprint
func (m *Manager) ConfigFingerprint() string {
	current, _, _ := m.switchIndex.Get()
	return m.namespaces[current].ConfigFingerprint()
}

// RecordSessionSQLMetrics record session SQL metrics, like response time, error
func (m *Manager) RecordSessionSQLMetrics(reqCtx *util.RequestContext, se *SessionExecutor, sql string, startTime time.Time, err error) {
	namespace := se.namespace
	ns := m.GetNamespace(namespace)
	if ns == nil {
		log.Warn("record session SQL metrics error, namespace: %s, sql: %s, err: %s", namespace, sql, "namespace not found")
		return
	}

	var operation string
	if stmtType := reqCtx.GetStmtType(); stmtType > -1 {
		operation = parser.StmtType(stmtType)
	} else {
		trimmedSql := strings.ReplaceAll(sql, "\n", " ")
		fingerprint := getSQLFingerprint(reqCtx, trimmedSql)
		operation = mysql.GetFingerprintOperation(fingerprint)
	}

	// record sql timing
	if !(err != nil && errs.Is(err, mysql.ErrClientQpsLimitedMsg)) {
		m.statistics.recordSessionSQLTiming(namespace, operation, startTime)
	}

	durationFloat := float64(time.Since(startTime).Microseconds()) / 1000.0

	if err == nil {
		se.manager.statistics.generalLogger.Notice("%s - %.1fms - ns=%s, %s@%s->%s/%s, connect_id=%d, mysql_connect_id=%d, transaction=%t|%v",
			SQLExecStatusOk, durationFloat, se.namespace, se.user, se.clientAddr, se.backendAddr, se.db,
			se.session.c.GetConnectionID(), se.backendConnectionId, se.isInTransaction(), sql)
	} else {
		// record error sql
		se.manager.statistics.generalLogger.Warn("%s - %.1fms - ns=%s, %s@%s->%s/%s, connect_id=%d, mysql_connect_id=%d, transaction=%t|%v. err:%s",
			SQLExecStatusErr, durationFloat, se.namespace, se.user, se.clientAddr, se.backendAddr, se.db,
			se.session.c.GetConnectionID(), se.backendConnectionId, se.isInTransaction(), sql, err)
		fingerprint := getSQLFingerprint(reqCtx, sql)
		md5 := getSQLFingerprintMd5(reqCtx, sql)
		ns.SetErrorSQLFingerprint(md5, fingerprint)
		m.statistics.recordSessionErrorSQLFingerprint(namespace, operation, md5)
	}

	// record slow sql, only durationFloat > slowSQLTime will be recorded
	if ns.getSessionSlowSQLTime() > 0 && int64(durationFloat) > ns.getSessionSlowSQLTime() {
		se.manager.statistics.generalLogger.Warn("%s - %.1fms - ns=%s, %s@%s->%s/%s, connect_id=%d, mysql_connect_id=%d, transaction=%t|%v",
			SQLExecStatusSlow, durationFloat, se.namespace, se.user, se.clientAddr, se.backendAddr, se.db,
			se.session.c.GetConnectionID(), se.backendConnectionId, se.isInTransaction(), sql)
		fingerprint := getSQLFingerprint(reqCtx, sql)
		md5 := getSQLFingerprintMd5(reqCtx, sql)
		ns.SetSlowSQLFingerprint(md5, fingerprint)
		m.statistics.recordSessionSlowSQLFingerprint(namespace, md5)
	}
}

// RecordBackendSQLMetrics record backend SQL metrics, like response time, error
func (m *Manager) RecordBackendSQLMetrics(reqCtx *util.RequestContext, se *SessionExecutor, sliceName, sql, backendAddr string, startTime time.Time, err error) {
	ns := m.GetNamespace(se.namespace)
	if ns == nil {
		log.Warn("record backend SQL metrics error, namespace: %s, backend addr: %s, sql: %s, err: %s", se.namespace, backendAddr, sql, "namespace not found")
		return
	}

	var operation string
	if stmtType := reqCtx.GetStmtType(); stmtType > -1 {
		operation = parser.StmtType(stmtType)
	} else {
		trimmedSql := strings.ReplaceAll(sql, "\n", " ")
		fingerprint := getSQLFingerprint(reqCtx, trimmedSql)
		operation = mysql.GetFingerprintOperation(fingerprint)
	}

	// record sql timing
	go m.statistics.recordBackendSQLTiming(se.namespace, operation, sliceName, backendAddr, startTime)

	// record backend slow sql
	duration := time.Since(startTime).Milliseconds()
	if m.statistics.isBackendSlowSQL(duration) {
		m.statistics.generalLogger.Warn("%s - %dms - ns=%s, %s@%s->%s/%s, connect_id=%d, mysql_connect_id=%d, transaction=%t|%v",
			SQLBackendExecStatusSlow, duration, se.namespace, se.user, se.clientAddr, se.backendAddr, se.db,
			se.session.c.GetConnectionID(), se.backendConnectionId, se.isInTransaction(), sql)
		fingerprint := getSQLFingerprint(reqCtx, sql)
		md5 := getSQLFingerprintMd5(reqCtx, sql)
		ns.SetBackendSlowSQLFingerprint(md5, fingerprint)
		m.statistics.recordBackendSlowSQLFingerprint(se.namespace, md5)
	}

	// record backend error sql
	if err != nil {
		m.statistics.generalLogger.Warn("%s - %dms - ns=%s, %s@%s->%s/%s, connect_id=%d, mysql_connect_id=%d, transaction=%t|%v, error: %v",
			SQLBackendExecStatusErr, duration, se.user, se.namespace, se.clientAddr, se.backendAddr, se.db,
			se.session.c.GetConnectionID(), se.backendConnectionId, se.isInTransaction(), sql, err)
		fingerprint := getSQLFingerprint(reqCtx, sql)
		md5 := getSQLFingerprintMd5(reqCtx, sql)
		ns.SetBackendErrorSQLFingerprint(md5, fingerprint)
		m.statistics.recordBackendErrorSQLFingerprint(se.namespace, operation, md5)
	}
}

func (m *Manager) startConnectPoolMetricsTask(interval int) {
	current, _, _ := m.switchIndex.Get()
	for _, ns := range m.namespaces[current].namespaces {
		m.statistics.SQLResponsePercentile[ns.name] = NewSQLResponse(ns.name)
	}

	if interval <= 0 {
		interval = 10
	}

	go func() {
		t := time.NewTicker(time.Duration(interval) * time.Second)
		tSQLRecordTime := time.NewTicker(time.Duration(backend.PingPeriod) * time.Second)
		for {
			select {
			case <-m.GetStatisticManager().closeChan:
				return
			case <-t.C:
				m.statistics.AddUptimeCount(time.Now().Unix() - m.statistics.startTime)

				// record cpu usage will wait at least 5 seconds
				m.statistics.CalcCPUBusy(interval - 5)

				current, _, _ := m.switchIndex.Get()
				for nameSpaceName := range m.namespaces[current].namespaces {
					m.recordBackendConnectPoolMetrics(nameSpaceName)
				}
			case <-tSQLRecordTime.C:
				m.statistics.CalcAvgSQLTimes()
			}
		}
	}()
}

func (m *Manager) recordBackendConnectPoolMetrics(namespace string) {
	ns := m.GetNamespace(namespace)
	if ns == nil {
		log.Warn("record backend connect pool metrics err, namespace: %s", namespace)
		return
	}
	for n, v := range m.statistics.SQLResponsePercentile {
		for backendAddr, val := range v.response99Max {
			m.statistics.recordBackendSQLTimingP99Max(n, backendAddr, int64(val))
		}
		for backendAddr, val := range v.response99Avg {
			m.statistics.recordBackendSQLTimingP99Avg(n, backendAddr, int64(val))
		}
		for backendAddr, val := range v.response95Max {
			m.statistics.recordBackendSQLTimingP95Max(n, backendAddr, int64(val))
		}
		for backendAddr, val := range v.response95Avg {
			m.statistics.recordBackendSQLTimingP95Avg(n, backendAddr, int64(val))
		}
	}

	for sliceName, slice := range ns.slices {
		m.statistics.recordInstanceDownCount(namespace, sliceName, slice.Master.ConnPool[0].Addr(), getStatusDownCounts(slice.Master.StatusMap, 0), MasterRole)
		m.statistics.recordConnectPoolInuseCount(namespace, sliceName, slice.Master.ConnPool[0].Addr(), slice.Master.ConnPool[0].InUse(), MasterRole)
		m.statistics.recordConnectPoolIdleCount(namespace, sliceName, slice.Master.ConnPool[0].Addr(), slice.Master.ConnPool[0].Available(), MasterRole)
		m.statistics.recordConnectPoolWaitCount(namespace, sliceName, slice.Master.ConnPool[0].Addr(), slice.Master.ConnPool[0].WaitCount(), MasterRole)
		m.statistics.recordConnectPoolActiveCount(namespace, sliceName, slice.Master.ConnPool[0].Addr(), slice.Master.ConnPool[0].Active(), MasterRole)
		m.statistics.recordConnectPoolCount(namespace, sliceName, slice.Master.ConnPool[0].Addr(), slice.Master.ConnPool[0].Capacity(), MasterRole)

		for i, slave := range slice.Slave.ConnPool {
			m.statistics.recordInstanceDownCount(namespace, sliceName, slave.Addr(), getStatusDownCounts(slice.Slave.StatusMap, i), SlaveRole)
			m.statistics.recordConnectPoolInuseCount(namespace, sliceName, slave.Addr(), slave.InUse(), SlaveRole)
			m.statistics.recordConnectPoolIdleCount(namespace, sliceName, slave.Addr(), slave.Available(), SlaveRole)
			m.statistics.recordConnectPoolWaitCount(namespace, sliceName, slave.Addr(), slave.WaitCount(), SlaveRole)
			m.statistics.recordConnectPoolActiveCount(namespace, sliceName, slave.Addr(), slave.Active(), SlaveRole)
			m.statistics.recordConnectPoolCount(namespace, sliceName, slave.Addr(), slave.Capacity(), SlaveRole)
		}
		for i, statisticSlave := range slice.StatisticSlave.ConnPool {
			m.statistics.recordInstanceDownCount(namespace, sliceName, statisticSlave.Addr(), getStatusDownCounts(slice.StatisticSlave.StatusMap, i), StatisticSlaveRole)
			m.statistics.recordConnectPoolInuseCount(namespace, sliceName, statisticSlave.Addr(), statisticSlave.InUse(), StatisticSlaveRole)
			m.statistics.recordConnectPoolIdleCount(namespace, sliceName, statisticSlave.Addr(), statisticSlave.Available(), StatisticSlaveRole)
			m.statistics.recordConnectPoolWaitCount(namespace, sliceName, statisticSlave.Addr(), statisticSlave.WaitCount(), StatisticSlaveRole)
			m.statistics.recordConnectPoolActiveCount(namespace, sliceName, statisticSlave.Addr(), statisticSlave.Active(), StatisticSlaveRole)
			m.statistics.recordConnectPoolCount(namespace, sliceName, statisticSlave.Addr(), statisticSlave.Capacity(), StatisticSlaveRole)
		}
	}
}

// NamespaceManager is the manager that holds all namespaces
type NamespaceManager struct {
	namespaces map[string]*Namespace
}

// NewNamespaceManager constructor of NamespaceManager
func NewNamespaceManager() *NamespaceManager {
	return &NamespaceManager{
		namespaces: make(map[string]*Namespace, 64),
	}
}

// CreateNamespaceManager create NamespaceManager
func CreateNamespaceManager(namespaceConfigs map[string]*models.Namespace) *NamespaceManager {
	nsMgr := NewNamespaceManager()
	proxyDatacenter, err := util.GetLocalDatacenter()
	if err != nil {
		log.Fatal("get proxy datacenter err,will use default datacenter,err:%s", err)
		proxyDatacenter = DefaultDatacenter
	}

	for _, config := range namespaceConfigs {
		namespace, err := NewNamespace(config, proxyDatacenter)
		if err != nil {
			log.Warn("create namespace %s failed, err: %v", config.Name, err)
			continue
		}
		nsMgr.namespaces[namespace.name] = namespace
	}
	return nsMgr
}

// ShallowCopyNamespaceManager copy NamespaceManager
func ShallowCopyNamespaceManager(nsMgr *NamespaceManager) *NamespaceManager {
	newNsMgr := NewNamespaceManager()
	maps.Copy(newNsMgr.namespaces, nsMgr.namespaces)
	return newNsMgr
}

// RebuildNamespace rebuild namespace
func (n *NamespaceManager) RebuildNamespace(config *models.Namespace) error {
	proxyDatacenter, err := util.GetLocalDatacenter()
	if err != nil {
		log.Fatal("get local proxy datacenter err:%s", err)
	}
	namespace, err := NewNamespace(config, proxyDatacenter)
	if err != nil {
		log.Warn("create namespace %s failed, err: %v", config.Name, err)
		return err
	}
	n.namespaces[config.Name] = namespace
	return nil
}

// DeleteNamespace delete namespace
func (n *NamespaceManager) DeleteNamespace(ns string) {
	delete(n.namespaces, ns)
}

// GetNamespace get namespace in NamespaceManager
func (n *NamespaceManager) GetNamespace(namespace string) *Namespace {
	return n.namespaces[namespace]
}

// GetNamespaces return all namespaces in NamespaceManager
func (n *NamespaceManager) GetNamespaces() map[string]*Namespace {
	return n.namespaces
}

// ConfigFingerprint return config fingerprint
func (n *NamespaceManager) ConfigFingerprint() string {
	sortedKeys := make([]string, 0)
	for k := range n.GetNamespaces() {
		sortedKeys = append(sortedKeys, k)
	}

	sort.Strings(sortedKeys)

	h := md5.New()
	for _, k := range sortedKeys {
		h.Write(n.GetNamespace(k).DumpToJSON())
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// UserManager means user for auth
// username+password是全局唯一的, 而username可以对应多个namespace
type UserManager struct {
	users          map[string][]string // key: user name, value: user password, same user may have different password, so array of passwords is needed
	userNamespaces map[string]string   // key: UserName+Password, value: name of namespace
}

// NewUserManager constructor of UserManager
func NewUserManager() *UserManager {
	return &UserManager{
		users:          make(map[string][]string, 64),
		userNamespaces: make(map[string]string, 64),
	}
}

// CreateUserManager create UserManager
func CreateUserManager(namespaceConfigs map[string]*models.Namespace) (*UserManager, error) {
	user := NewUserManager()
	for _, ns := range namespaceConfigs {
		user.addNamespaceUsers(ns)
	}
	return user, nil
}

// CloneUserManager close UserManager
func CloneUserManager(user *UserManager) *UserManager {
	ret := NewUserManager()
	// copy
	maps.Copy(ret.userNamespaces, user.userNamespaces)
	for k, v := range user.users {
		users := make([]string, len(v))
		copy(users, v)
		ret.users[k] = users
	}

	return ret
}

// RebuildNamespaceUsers rebuild users in namespace
func (u *UserManager) RebuildNamespaceUsers(namespace *models.Namespace) {
	u.ClearNamespaceUsers(namespace.Name)
	u.addNamespaceUsers(namespace)
}

// ClearNamespaceUsers clear users in namespace
func (u *UserManager) ClearNamespaceUsers(namespace string) {
	for key, ns := range u.userNamespaces {
		if ns == namespace {
			delete(u.userNamespaces, key)

			// delete user password in users
			username, password := getUserAndPasswordFromKey(key)
			var s []string
			for i := range u.users[username] {
				if u.users[username][i] == password {
					s = append(u.users[username][:i], u.users[username][i+1:]...)
				}
			}
			u.users[username] = s
		}
	}
}

func (u *UserManager) addNamespaceUsers(namespace *models.Namespace) {
	for _, user := range namespace.Users {
		key := getUserKey(user.UserName, user.Password)
		u.userNamespaces[key] = namespace.Name
		u.users[user.UserName] = append(u.users[user.UserName], user.Password)
	}
}

// CheckUser check if user in users
func (u *UserManager) CheckUser(user string) bool {
	if _, ok := u.users[user]; ok {
		return true
	}
	return false
}

// CheckPassword check if right password with specific user
func (u *UserManager) CheckPassword(user string, salt, auth []byte) (bool, string) {
	for _, password := range u.users[user] {
		checkAuth := mysql.CalcPassword(salt, []byte(password))
		if bytes.Equal(auth, checkAuth) {
			return true, password
		}
	}
	return false, ""
}

// CheckHashPassword check encrypt password with specific user
func (u *UserManager) CheckHashPassword(user string, salt, auth []byte) (bool, string) {
	for _, password := range u.users[user] {
		if strings.HasPrefix(password, "*") && len(password) == 41 {
			if mysql.CheckHashPassword(auth, salt, []byte(password)[1:]) {
				return true, password
			}
		}
	}
	return false, ""
}

// CheckPassword check if right password with specific user
func (u *UserManager) CheckSha2Password(user string, salt, auth []byte) (bool, string) {
	for _, password := range u.users[user] {
		checkAuth := mysql.CalcCachingSha2Password(salt, password)
		if bytes.Equal(auth, checkAuth) {
			return true, password
		}
	}
	return false, ""
}

// GetNamespaceByUser return namespace by user
func (u *UserManager) GetNamespaceByUser(userName, password string) string {
	key := getUserKey(userName, password)
	if name, ok := u.userNamespaces[key]; ok {
		return name
	}
	return ""
}

func getUserKey(username, password string) string {
	return username + ":" + password
}

func getUserAndPasswordFromKey(key string) (username string, password string) {
	strs := strings.Split(key, ":")
	return strs[0], strs[1]
}

const (
	statsLabelCluster       = "Cluster"
	statsLabelOperation     = "Operation"
	statsLabelNamespace     = "Namespace"
	statsLabelFingerprint   = "Fingerprint"
	statsLabelFlowDirection = "Flowdirection"
	statsLabelSlice         = "Slice"
	statsLabelIPAddr        = "IPAddr"
	statsLabelRole          = "role"
)

// StatisticManager statistics manager
type StatisticManager struct {
	manager     *Manager
	clusterName string
	startTime   int64

	statsType     string // 监控后端类型
	handlers      map[string]http.Handler
	generalLogger log.Logger

	sqlTimings                *stats.MultiTimings            // SQL耗时统计
	sqlFingerprintSlowCounts  *stats.CountersWithMultiLabels // 慢SQL指纹数量统计
	sqlErrorCounts            *stats.CountersWithMultiLabels // SQL错误数统计
	sqlFingerprintErrorCounts *stats.CountersWithMultiLabels // SQL指纹错误数统计
	sqlForbidenCounts         *stats.CountersWithMultiLabels // SQL黑名单请求统计
	flowCounts                *stats.CountersWithMultiLabels // 业务流量统计
	sessionCounts             *stats.GaugesWithMultiLabels   // 前端会话数统计
	CPUBusy                   *stats.GaugesWithMultiLabels   // Gaea服务器CPU消耗情况
	clientConnecions          sync.Map                       // 等同于sessionCounts, 用于限制前端连接

	backendSQLTimings                *stats.MultiTimings            // 后端SQL耗时统计
	backendSQLFingerprintSlowCounts  *stats.CountersWithMultiLabels // 后端慢SQL指纹数量统计
	backendSQLErrorCounts            *stats.CountersWithMultiLabels // 后端SQL错误数统计
	backendSQLFingerprintErrorCounts *stats.CountersWithMultiLabels // 后端SQL指纹错误数统计
	backendConnectPoolIdleCounts     *stats.GaugesWithMultiLabels   // 后端空闲连接数统计
	backendConnectPoolInUseCounts    *stats.GaugesWithMultiLabels   // 后端正在使用连接数统计
	backendConnectPoolActiveCounts   *stats.GaugesWithMultiLabels   // 后端活跃连接数统计
	backendConnectPoolWaitCounts     *stats.GaugesWithMultiLabels   // 后端等待队列统计
	backendConnectPoolCapacityCounts *stats.GaugesWithMultiLabels   // 当前连接池大小
	backendInstanceDownCounts        *stats.GaugesWithMultiLabels   // 后端实例状态统计
	uptimeCounts                     *stats.GaugesWithMultiLabels   // 启动时间记录
	backendSQLResponse99MaxCounts    *stats.GaugesWithMultiLabels   // 后端 SQL 耗时 P99 最大响应时间
	backendSQLResponse99AvgCounts    *stats.GaugesWithMultiLabels   // 后端 SQL 耗时 P99 平均响应时间
	backendSQLResponse95MaxCounts    *stats.GaugesWithMultiLabels   // 后端 SQL 耗时 P95 最大响应时间
	backendSQLResponse95AvgCounts    *stats.GaugesWithMultiLabels   // 后端 SQL 耗时 P95 平均响应时间

	SQLResponsePercentile map[string]*SQLResponse // 用于记录 P99/P95 Max/AVG 响应时间
	slowSQLTime           int64
	CPUNums               int // Gaea服务器使用的CPU核数
	closeChan             chan bool
}

// SQLResponse record one namespace SQL response like P99/P95
type SQLResponse struct {
	ns                      string
	sqlExecTimeRecordSwitch bool
	sQLExecTimeChan         chan *SQLExecTimeRecord
	sQLTimeList             []*SQLExecTimeRecord
	response99Max           map[string]int64 // map[backendAddr]P99MaxValue
	response99Avg           map[string]int64 // map[backendAddr]P99AvgValue
	response95Max           map[string]int64 // map[backendAddr]P95MaxValue
	response95Avg           map[string]int64 // map[backendAddr]P95AvgValue
}

// SQLExecTimeRecord record backend sql exec time
type SQLExecTimeRecord struct {
	sliceName     string
	backendAddr   string
	execTimeMicro int64
}

func NewSQLResponse(name string) *SQLResponse {
	sQLExecTimeRecord := make([]*SQLExecTimeRecord, 0, SQLExecTimeSize)
	return &SQLResponse{
		ns:                      name,
		sqlExecTimeRecordSwitch: false,
		sQLExecTimeChan:         make(chan *SQLExecTimeRecord, SQLExecTimeSize),
		sQLTimeList:             sQLExecTimeRecord,
		response99Max:           make(map[string]int64),
		response99Avg:           make(map[string]int64),
		response95Max:           make(map[string]int64),
		response95Avg:           make(map[string]int64),
	}

}

// NewStatisticManager return empty StatisticManager
func NewStatisticManager() *StatisticManager {
	return &StatisticManager{}
}

// CreateStatisticManager create StatisticManager
func CreateStatisticManager(cfg *models.Proxy, manager *Manager) (*StatisticManager, error) {
	mgr := NewStatisticManager()
	mgr.manager = manager
	mgr.clusterName = cfg.Cluster
	mgr.SQLResponsePercentile = make(map[string]*SQLResponse)
	mgr.CPUNums = cfg.NumCPU

	var err error
	if err = mgr.Init(cfg); err != nil {
		return nil, err
	}
	if mgr.generalLogger, err = initGeneralLogger(cfg); err != nil {
		return nil, err
	}
	return mgr, nil
}

type proxyStatsConfig struct {
	Service      string
	StatsEnabled bool
}

func initGeneralLogger(cfg *models.Proxy) (log.Logger, error) {
	c := make(map[string]string, 5)
	c["path"] = cfg.LogPath
	c["filename"] = cfg.LogFileName + "_sql"
	c["level"] = cfg.LogLevel
	c["service"] = cfg.Service
	c["runtime"] = "false"

	// LogKeepDays 或者 LogKeepCounts 只配置一个且大于默认值，实际日志保留天数为配置的天数
	if cfg.LogKeepDays > log.DefaultLogKeepDays && cfg.LogKeepCounts == 0 {
		cfg.LogKeepCounts = cfg.LogKeepDays * 24
	}
	if cfg.LogKeepCounts > log.DefaultLogKeepCounts && cfg.LogKeepDays == 0 {
		cfg.LogKeepDays = int(math.Ceil(float64(cfg.LogKeepCounts) / 24))
	}

	// 若配置的保留天数小于默认值，实际日志保留天数为配置的天数
	c["log_keep_days"] = strconv.Itoa(log.DefaultLogKeepDays)
	if cfg.LogKeepDays != 0 {
		c["log_keep_days"] = strconv.Itoa(cfg.LogKeepDays)
	}

	c["log_keep_counts"] = strconv.Itoa(log.DefaultLogKeepCounts)
	if cfg.LogKeepCounts != 0 {
		c["log_keep_counts"] = strconv.Itoa(cfg.LogKeepCounts)
	}

	return zap.CreateLogManager(c)
}

func parseProxyStatsConfig(cfg *models.Proxy) (*proxyStatsConfig, error) {
	enabled, err := strconv.ParseBool(cfg.StatsEnabled)
	if err != nil {
		return nil, err
	}

	statsConfig := &proxyStatsConfig{
		Service:      cfg.Service,
		StatsEnabled: enabled,
	}
	return statsConfig, nil
}

// Init init StatisticManager
func (s *StatisticManager) Init(cfg *models.Proxy) error {
	s.startTime = time.Now().Unix()
	s.closeChan = make(chan bool)
	s.handlers = make(map[string]http.Handler)
	s.slowSQLTime = cfg.SlowSQLTime
	s.CPUNums = cfg.NumCPU
	statsCfg, err := parseProxyStatsConfig(cfg)
	if err != nil {
		return err
	}

	if err := s.initBackend(statsCfg); err != nil {
		return err
	}

	s.sqlTimings = stats.NewMultiTimings("SqlTimings",
		"gaea proxy sql sqlTimings", []string{statsLabelCluster, statsLabelNamespace, statsLabelOperation})
	s.sqlFingerprintSlowCounts = stats.NewCountersWithMultiLabels("SqlFingerprintSlowCounts",
		"gaea proxy sql fingerprint slow counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelFingerprint})
	s.sqlErrorCounts = stats.NewCountersWithMultiLabels("SqlErrorCounts",
		"gaea proxy sql error counts per error type", []string{statsLabelCluster, statsLabelNamespace, statsLabelOperation})
	s.sqlFingerprintErrorCounts = stats.NewCountersWithMultiLabels("SqlFingerprintErrorCounts",
		"gaea proxy sql fingerprint error counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelFingerprint})
	s.sqlForbidenCounts = stats.NewCountersWithMultiLabels("SqlForbiddenCounts",
		"gaea proxy sql error counts per error type", []string{statsLabelCluster, statsLabelNamespace, statsLabelFingerprint})
	s.flowCounts = stats.NewCountersWithMultiLabels("FlowCounts",
		"gaea proxy flow counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelFlowDirection})
	s.sessionCounts = stats.NewGaugesWithMultiLabels("SessionCounts",
		"gaea proxy session counts", []string{statsLabelCluster, statsLabelNamespace})
	s.CPUBusy = stats.NewGaugesWithMultiLabels("CPUBusyByCore", "gaea proxy CPU busy by core", []string{statsLabelCluster})

	s.backendSQLTimings = stats.NewMultiTimings("BackendSqlTimings",
		"gaea proxy backend sql sqlTimings", []string{statsLabelCluster, statsLabelNamespace, statsLabelOperation})
	s.backendSQLFingerprintSlowCounts = stats.NewCountersWithMultiLabels("BackendSqlFingerprintSlowCounts",
		"gaea proxy backend sql fingerprint slow counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelFingerprint})
	s.backendSQLErrorCounts = stats.NewCountersWithMultiLabels("BackendSqlErrorCounts",
		"gaea proxy backend sql error counts per error type", []string{statsLabelCluster, statsLabelNamespace, statsLabelOperation})
	s.backendSQLFingerprintErrorCounts = stats.NewCountersWithMultiLabels("BackendSqlFingerprintErrorCounts",
		"gaea proxy backend sql fingerprint error counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelFingerprint})
	s.backendConnectPoolIdleCounts = stats.NewGaugesWithMultiLabels("backendConnectPoolIdleCounts",
		"gaea proxy backend idle connect counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelSlice, statsLabelIPAddr, statsLabelRole})
	s.backendConnectPoolInUseCounts = stats.NewGaugesWithMultiLabels("backendConnectPoolInUseCounts",
		"gaea proxy backend in-use connect counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelSlice, statsLabelIPAddr, statsLabelRole})
	s.backendConnectPoolWaitCounts = stats.NewGaugesWithMultiLabels("backendConnectPoolWaitCounts",
		"gaea proxy backend wait connect counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelSlice, statsLabelIPAddr, statsLabelRole})
	s.backendConnectPoolActiveCounts = stats.NewGaugesWithMultiLabels("backendConnectPoolActiveCounts",
		"gaea proxy backend active connect counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelSlice, statsLabelIPAddr, statsLabelRole})
	s.backendConnectPoolCapacityCounts = stats.NewGaugesWithMultiLabels("backendConnectPoolCapacityCounts",
		"gaea proxy backend capacity connect counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelSlice, statsLabelIPAddr, statsLabelRole})
	s.backendInstanceDownCounts = stats.NewGaugesWithMultiLabels("backendInstanceDownCounts",
		"gaea proxy backend DB status down counts", []string{statsLabelCluster, statsLabelNamespace, statsLabelSlice, statsLabelIPAddr, statsLabelRole})
	s.backendSQLResponse99MaxCounts = stats.NewGaugesWithMultiLabels("backendSQLResponse99MaxCounts",
		"gaea proxy backend sql sqlTimings P99 max", []string{statsLabelCluster, statsLabelNamespace, statsLabelIPAddr})
	s.backendSQLResponse99AvgCounts = stats.NewGaugesWithMultiLabels("backendSQLResponse99AvgCounts",
		"gaea proxy backend sql sqlTimings P99 avg", []string{statsLabelCluster, statsLabelNamespace, statsLabelIPAddr})
	s.backendSQLResponse95MaxCounts = stats.NewGaugesWithMultiLabels("backendSQLResponse95MaxCounts",
		"gaea proxy backend sql sqlTimings P95 max", []string{statsLabelCluster, statsLabelNamespace, statsLabelIPAddr})
	s.backendSQLResponse95AvgCounts = stats.NewGaugesWithMultiLabels("backendSQLResponse95AvgCounts",
		"gaea proxy backend sql sqlTimings P95 avg", []string{statsLabelCluster, statsLabelNamespace, statsLabelIPAddr})
	s.uptimeCounts = stats.NewGaugesWithMultiLabels("UptimeCounts",
		"gaea proxy uptime counts", []string{statsLabelCluster})
	s.clientConnecions = sync.Map{}
	s.startClearTask()
	return nil
}

// Close close proxy stats
func (s *StatisticManager) Close() {
	close(s.closeChan)
}

// GetHandlers return specific handler of stats
func (s *StatisticManager) GetHandlers() map[string]http.Handler {
	return s.handlers
}

func (s *StatisticManager) initBackend(cfg *proxyStatsConfig) error {
	prometheus.Init(cfg.Service)
	s.handlers = prometheus.GetHandlers()
	return nil
}

// clear data to prevent
func (s *StatisticManager) startClearTask() {
	go func() {
		t := time.NewTicker(time.Hour)
		for {
			select {
			case <-s.closeChan:
				return
			case <-t.C:
				s.clearLargeCounters()
			}
		}
	}()
}

func (s *StatisticManager) clearLargeCounters() {
	s.sqlErrorCounts.ResetAll()
	s.sqlFingerprintSlowCounts.ResetAll()
	s.sqlFingerprintErrorCounts.ResetAll()

	s.backendSQLErrorCounts.ResetAll()
	s.backendSQLFingerprintSlowCounts.ResetAll()
	s.backendSQLFingerprintErrorCounts.ResetAll()
}

func (s *StatisticManager) recordSessionSlowSQLFingerprint(namespace string, md5 string) {
	fingerprintStatsKey := []string{s.clusterName, namespace, md5}
	s.sqlFingerprintSlowCounts.Add(fingerprintStatsKey, 1)
}

func (s *StatisticManager) recordSessionErrorSQLFingerprint(namespace string, operation string, md5 string) {
	fingerprintStatsKey := []string{s.clusterName, namespace, md5}
	operationStatsKey := []string{s.clusterName, namespace, operation}
	s.sqlErrorCounts.Add(operationStatsKey, 1)
	s.sqlFingerprintErrorCounts.Add(fingerprintStatsKey, 1)
}

func (s *StatisticManager) recordSessionSQLTiming(namespace string, operation string, startTime time.Time) {
	operationStatsKey := []string{s.clusterName, namespace, operation}
	s.sqlTimings.Record(operationStatsKey, startTime)
}

// isBackendSlowSQL return true only gaea.ini slow_sql_time > 0 and duration > slow_sql_time
func (s *StatisticManager) isBackendSlowSQL(duration int64) bool {
	return s.slowSQLTime > 0 && duration > s.slowSQLTime
}

func (s *StatisticManager) recordBackendSlowSQLFingerprint(namespace string, md5 string) {
	fingerprintStatsKey := []string{s.clusterName, namespace, md5}
	s.backendSQLFingerprintSlowCounts.Add(fingerprintStatsKey, 1)
}

func (s *StatisticManager) recordBackendErrorSQLFingerprint(namespace string, operation string, md5 string) {
	fingerprintStatsKey := []string{s.clusterName, namespace, md5}
	operationStatsKey := []string{s.clusterName, namespace, operation}
	s.backendSQLErrorCounts.Add(operationStatsKey, 1)
	s.backendSQLFingerprintErrorCounts.Add(fingerprintStatsKey, 1)
}

func (s *StatisticManager) recordBackendSQLTiming(namespace string, operation string, sliceName, backendAddr string, startTime time.Time) {
	operationStatsKey := []string{s.clusterName, namespace, operation}
	s.backendSQLTimings.Record(operationStatsKey, startTime)

	if s.SQLResponsePercentile[namespace] == nil {
		log.Warn("ns %s not in SQLResponsePercentile", namespace)
		return
	}
	if !s.SQLResponsePercentile[namespace].sqlExecTimeRecordSwitch {
		return
	}
	execTimeMicro := time.Since(startTime).Microseconds()
	sQLExecTimeRecord := &SQLExecTimeRecord{
		sliceName:     sliceName,
		backendAddr:   backendAddr,
		execTimeMicro: execTimeMicro,
	}
	select {
	case s.SQLResponsePercentile[namespace].sQLExecTimeChan <- sQLExecTimeRecord:
	case <-time.After(time.Millisecond):
		s.SQLResponsePercentile[namespace].sqlExecTimeRecordSwitch = false
	}
}

// RecordSQLForbidden record forbidden sql
func (s *StatisticManager) RecordSQLForbidden(fingerprint, namespace string) {
	md5 := mysql.GetMd5(fingerprint)
	s.sqlForbidenCounts.Add([]string{s.clusterName, namespace, md5}, 1)
}

// IncrSessionCount incr session count
func (s *StatisticManager) IncrSessionCount(namespace string) {
	statsKey := []string{s.clusterName, namespace}
	s.sessionCounts.Add(statsKey, 1)
}

func (s *StatisticManager) IncrConnectionCount(namespace string) {
	if value, ok := s.clientConnecions.Load(namespace); !ok {
		s.clientConnecions.Store(namespace, atomic.NewInt32(1))
	} else {
		lastNum := value.(*atomic.Int32)
		lastNum.Inc()
	}
}

// DescSessionCount decr session count
func (s *StatisticManager) DescSessionCount(namespace string) {
	statsKey := []string{s.clusterName, namespace}
	s.sessionCounts.Add(statsKey, -1)
}

func (s *StatisticManager) DescConnectionCount(namespace string) {
	if value, ok := s.clientConnecions.Load(namespace); !ok {
		_ = log.Warn("namespace: '%v' maxClientConnections should in map", namespace)
	} else {
		lastNum := value.(*atomic.Int32)
		lastNum.Dec()
	}
}

// AddReadFlowCount add read flow count
func (s *StatisticManager) AddReadFlowCount(namespace string, byteCount int) {
	statsKey := []string{s.clusterName, namespace, "read"}
	s.flowCounts.Add(statsKey, int64(byteCount))
}

// AddWriteFlowCount add write flow count
func (s *StatisticManager) AddWriteFlowCount(namespace string, byteCount int) {
	statsKey := []string{s.clusterName, namespace, "write"}
	s.flowCounts.Add(statsKey, int64(byteCount))
}

// record idle connect count
func (s *StatisticManager) recordConnectPoolIdleCount(namespace string, slice string, addr string, count int64, role string) {
	statsKey := []string{s.clusterName, namespace, slice, addr, role}
	s.backendConnectPoolIdleCounts.Set(statsKey, count)
}

// record in-use connect count
func (s *StatisticManager) recordConnectPoolInuseCount(namespace string, slice string, addr string, count int64, role string) {
	statsKey := []string{s.clusterName, namespace, slice, addr, role}
	s.backendConnectPoolInUseCounts.Set(statsKey, count)
}

// record wait queue length
func (s *StatisticManager) recordConnectPoolWaitCount(namespace string, slice string, addr string, count int64, role string) {
	statsKey := []string{s.clusterName, namespace, slice, addr, role}
	s.backendConnectPoolWaitCounts.Set(statsKey, count)
}

// recordConnectPoolActive records the count of active connections in a connection pool for a specific server role within a namespace and slice context.
func (s *StatisticManager) recordConnectPoolActiveCount(namespace string, slice string, addr string, count int64, role string) {
	statsKey := []string{s.clusterName, namespace, slice, addr, role}
	s.backendConnectPoolActiveCounts.Set(statsKey, count)
}

// recordConnectPoolCount records the total capacity of a connection pool for a specific server role within a namespace and slice context.
func (s *StatisticManager) recordConnectPoolCount(namespace string, slice string, addr string, count int64, role string) {
	statsKey := []string{s.clusterName, namespace, slice, addr, role}
	s.backendConnectPoolCapacityCounts.Set(statsKey, count)
}

// record wait queue length
func (s *StatisticManager) recordInstanceDownCount(namespace string, slice string, addr string, count int64, role string) {
	statsKey := []string{s.clusterName, namespace, slice, addr, role}
	s.backendInstanceDownCounts.Set(statsKey, count)
}

// record wait queue length
func (s *StatisticManager) recordBackendSQLTimingP99Max(namespace, backendAddr string, count int64) {
	statsKey := []string{s.clusterName, namespace, backendAddr}
	s.backendSQLResponse99MaxCounts.Set(statsKey, count)
}

func (s *StatisticManager) recordBackendSQLTimingP99Avg(namespace, backendAddr string, count int64) {
	statsKey := []string{s.clusterName, namespace, backendAddr}
	s.backendSQLResponse99AvgCounts.Set(statsKey, count)
}

func (s *StatisticManager) recordBackendSQLTimingP95Max(namespace, backendAddr string, count int64) {
	statsKey := []string{s.clusterName, namespace, backendAddr}
	s.backendSQLResponse95MaxCounts.Set(statsKey, count)
}

func (s *StatisticManager) recordBackendSQLTimingP95Avg(namespace, backendAddr string, count int64) {
	statsKey := []string{s.clusterName, namespace, backendAddr}
	s.backendSQLResponse95AvgCounts.Set(statsKey, count)
}

// AddUptimeCount add uptime count
func (s *StatisticManager) AddUptimeCount(count int64) {
	statsKey := []string{s.clusterName}
	s.uptimeCounts.Set(statsKey, count)
}

func (s *StatisticManager) CalcCPUBusy(interval int) {
	cpuBusy := int64(0)
	p, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		log.Notice("server", "gopsutil", "NewProcess", 0, err)
		return
	}
	cpuPercent, err := p.Percent(time.Duration(interval) * time.Second)
	if err == nil {
		if s.CPUNums != 0 {
			// 为了适应CountersWithMultiLabels的数据类型，这里对cpuTime结果做了取整，grafana显示时需要还原
			cpuBusy = int64(cpuPercent / float64(s.CPUNums) * 100)

		} else {
			cpuBusy = int64(cpuPercent / float64(runtime.NumCPU()) * 100)
		}
	}
	statsKey := []string{s.clusterName}
	s.CPUBusy.Set(statsKey, cpuBusy)
}

func (s *StatisticManager) CalcAvgSQLTimes() {
	for ns, sQLResponse := range s.SQLResponsePercentile {
		sqlTimesMicro := make([]int64, 0)
		quit := false
		backendAddr := ""
		for !quit {
			select {
			case tmp := <-sQLResponse.sQLExecTimeChan:
				if len(sqlTimesMicro) >= SQLExecTimeSize {
					quit = true
				}
				backendAddr = tmp.backendAddr
				etime := tmp.execTimeMicro
				sqlTimesMicro = append(sqlTimesMicro, etime)
			case <-time.After(time.Millisecond):
				quit = true
			}
		}
		if len(sqlTimesMicro) == 0 {
			s.SQLResponsePercentile[ns].response99Max[backendAddr] = 0
			s.SQLResponsePercentile[ns].response95Max[backendAddr] = 0
			s.SQLResponsePercentile[ns].response99Avg[backendAddr] = 0
			s.SQLResponsePercentile[ns].response95Avg[backendAddr] = 0
			s.SQLResponsePercentile[ns].sqlExecTimeRecordSwitch = true
			continue
		}
		sort.Slice(sqlTimesMicro, func(i, j int) bool { return sqlTimesMicro[i] < sqlTimesMicro[j] })
		sum := int64(0)
		p99sum := int64(0)
		p95sum := int64(0)
		s.SQLResponsePercentile[ns].response99Max[backendAddr] = sqlTimesMicro[(len(sqlTimesMicro)-1)*99/100]
		s.SQLResponsePercentile[ns].response95Max[backendAddr] = sqlTimesMicro[(len(sqlTimesMicro)-1)*95/100]
		for k := range sqlTimesMicro {
			sum += sqlTimesMicro[k]
			if k < len(sqlTimesMicro)*95/100 {
				p95sum += sqlTimesMicro[k]
			}
			if k < len(sqlTimesMicro)*99/100 {
				p99sum += sqlTimesMicro[k]
			}
		}
		if len(sqlTimesMicro)*99/100 > 0 {
			s.SQLResponsePercentile[ns].response99Avg[backendAddr] = p99sum / int64(len(sqlTimesMicro)*99/100)
		}
		if len(sqlTimesMicro)*95/100 > 0 {
			s.SQLResponsePercentile[ns].response95Avg[backendAddr] = p95sum / int64(len(sqlTimesMicro)*95/100)
		}
		s.SQLResponsePercentile[ns].sqlExecTimeRecordSwitch = true
	}
}

// getStatusDownCounts get status down counts from DBinfo.statusMap
func getStatusDownCounts(statusMap *sync.Map, index int) int64 {
	if v, ok := statusMap.Load(index); !ok {
		return 1
	} else if v != backend.StatusUp {
		return 1
	}
	return 0
}
