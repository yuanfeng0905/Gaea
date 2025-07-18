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
	"encoding/binary"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"time"

	"github.com/XiaoMi/Gaea/core/errors"

	"github.com/XiaoMi/Gaea/backend"
	"github.com/XiaoMi/Gaea/log"
	"github.com/XiaoMi/Gaea/mysql"
	"github.com/XiaoMi/Gaea/parser"
	"github.com/XiaoMi/Gaea/parser/ast"
	"github.com/XiaoMi/Gaea/proxy/plan"
	"github.com/XiaoMi/Gaea/util"
)

const (
	size = 4096
)

// Parse parse sql
func (se *SessionExecutor) Parse(sql string) (ast.StmtNode, error) {
	return parser.New().ParseOneStmt(sql, "", "")
}

// 处理query语句
func (se *SessionExecutor) handleQuery(sql string) (r *mysql.Result, err error) {
	defer func() {
		if e := recover(); e != nil {
			log.Warn("handle query command failed, error: %v, sql: %s", e, sql)

			if err, ok := e.(error); ok {
				buf := make([]byte, size)
				buf = buf[:runtime.Stack(buf, false)]

				log.Warn("handle query command catch panic error, sql: %s, error: %s, stack: %s",
					sql, err.Error(), string(buf))
			}

			err = fmt.Errorf("%s:%s", errors.ErrInternalServer, e)
			return
		}
	}()

	sql = strings.TrimRight(sql, ";") //删除sql语句最后的分号
	reqCtx := util.NewRequestContext()
	ns := se.GetNamespace()
	startTime := time.Now()

	// clientQPSLimit: max client queries per second
	// supportLimitTx: limit transaction queries
	// qps limit error code: 901
	if se.GetNamespace().clientQPSLimit > 0 && se.GetNamespace().supportLimitTx && !se.GetNamespace().limiter.Allow() {
		if se.isInTransaction() {
			// if client transaction is limited, gaea will actively close client connection
			err = mysql.NewSessionCloseRespErrorWithErr(mysql.ErrClientQpsLimitedMsg)
		} else {
			// if non-transaction connection is limited, gaea will not close client connection
			err = mysql.ErrClientQpsLimitedMsg
		}
	} else if se.GetNamespace().clientQPSLimit > 0 && !se.GetNamespace().supportLimitTx && !se.isInTransaction() && !se.GetNamespace().limiter.Allow() {
		// if non-transaction connection is limited, gaea will not close client connection
		err = mysql.ErrClientQpsLimitedMsg
	} else {
		if ns.supportMultiQuery && se.session.c.capability&mysql.ClientMultiStatements != 0 {
			r, err = se.doMultiStmts(reqCtx, sql)
		} else {
			r, err = se.doQuery(reqCtx, sql)
		}
	}

	se.manager.RecordSessionSQLMetrics(reqCtx, se, sql, startTime, err)
	return r, err
}

func (se *SessionExecutor) checkSQLAllowed(reqCtx *util.RequestContext, sql string) error {
	stmtType := parser.Preview(sql)
	reqCtx.SetStmtType(stmtType)
	if isSQLNotAllowedByUser(se, stmtType) {
		return fmt.Errorf("write DML is now allowed by read user")
	}
	ns := se.GetNamespace()
	if !ns.IsSQLAllowed(reqCtx, sql) {
		fingerprint := getSQLFingerprint(reqCtx, sql)
		log.Warn("catch black sql, sql: %s", sql)
		se.manager.GetStatisticManager().RecordSQLForbidden(fingerprint, se.GetNamespace().GetName())
		err := mysql.NewError(mysql.ErrUnknown, "sql in blacklist")
		return err
	}
	return nil
}

// handle multi-stmts,like `select 1;set autcommit=0;insert into...;`
func (se *SessionExecutor) doMultiStmts(reqCtx *util.RequestContext, sql string) (r *mysql.Result, errRet error) {
	if se.session.c.hasRecycledReadPacket.CompareAndSwap(false, true) {
		se.session.c.RecycleReadPacket()
	}

	piecesSql, err := parser.SplitStatementToPieces(sql)
	if err != nil {
		log.Warn("parse sql error. sql: [%s], err: %v", sql, err)
		return nil, err
	}

	stmtsNum := len(piecesSql)
	if stmtsNum == 1 { //single statements
		return se.doQuery(reqCtx, sql)
	}

	//multi-query
	for index, piece := range piecesSql {
		setContextSQLFingerprint(reqCtx, sql)
		r, errRet = se.doQuery(reqCtx, piece)
		if errRet != nil {
			return nil, errRet
		}

		if index < stmtsNum-1 {
			//write result to client
			response := CreateResultResponse(se.status|mysql.ServerMoreResultsExists, r, false)
			if err = se.session.writeResponse(response); err != nil {
				log.Warn("session write response error, error: %v", err)
				se.session.Close()
				return r, err
			}
		}
	}

	return r, errRet
}

func (se *SessionExecutor) doQuery(reqCtx *util.RequestContext, sql string) (*mysql.Result, error) {
	if err := se.checkSQLAllowed(reqCtx, sql); err != nil {
		return nil, err
	}

	if canHandleWithoutPlan(reqCtx.GetStmtType()) {
		return se.handleQueryWithoutPlan(reqCtx, sql)
	}

	db := se.db
	if se.session == nil {
		return nil, fmt.Errorf("session is nil")
	}

	// get plan 会生成 tokens，需要放在 checkExecuteFromSlave 前面
	slog.Info(fmt.Sprintf("get plan for ns:%s, sql: %s, db: %s", se.GetNamespace().GetName(), sql, db))
	p, err := se.getPlan(reqCtx, se.GetNamespace(), db, sql, true)
	if err != nil {
		return nil, fmt.Errorf("get plan error, db: %s, origin sql: %s, err: %v", db, sql, err)
	}

	// 防止多语句执行的时候被复用
	if checkExecuteFromSlave(reqCtx, se, sql) {
		reqCtx.SetFromSlave(1)
	} else {
		reqCtx.SetFromSlave(0)
	}

	reqCtx.SetDefaultSlice(se.GetNamespace().GetDefaultSlice())
	r, err := p.ExecuteIn(reqCtx, se)
	if err != nil {
		return nil, err
	}

	modifyResultStatus(r, se)

	return r, nil
}

func checkMyCatHintPlan(reqCtx *util.RequestContext, se *SessionExecutor, db string, comments parser.MarginComments) (plan.Plan, error) {
	if !strings.HasPrefix(strings.TrimSpace(comments.Trailing), mycatHint) {
		return nil, nil
	}

	hintSQL := getMyCatHintSQL(comments.Trailing)
	if hintSQL == "" {
		return nil, fmt.Errorf("get nil hintSQL.comments:%+v", comments)
	}
	hintPlan, err := se.getPlan(reqCtx, se.GetNamespace(), db, hintSQL, false)
	if err != nil {
		return nil, fmt.Errorf("get MyCat hintPlan error")
	}
	return hintPlan, nil
}

// 处理逻辑较简单的SQL, 不走执行计划部分
func (se *SessionExecutor) handleQueryWithoutPlan(reqCtx *util.RequestContext, sql string) (*mysql.Result, error) {
	tokens := parser.Tokenize(sql)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("tokenize sql error, sql: %s", sql)
	}
	reqCtx.SetTokens(tokens)

	stmtType := reqCtx.GetStmtType()

	// handle show to prevent parsing error in some cases like sql mode has ANSI_QUOTES
	if stmtType == parser.StmtShow {
		return se.handleShow(reqCtx, sql)
	}

	n, err := se.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("parse sql error, sql: %s, err: %v", sql, err)
	}

	switch stmt := n.(type) {
	case *ast.SetStmt:
		return se.handleSet(reqCtx, sql, stmt)
	case *ast.BeginStmt:
		return nil, se.handleBegin()
	case *ast.CommitStmt:
		return nil, se.handleCommit()
	case *ast.LockTablesStmt:
		// TODO: handle lock tables
		// TODO: unify sql exec time
		se.manager.statistics.generalLogger.Warn("%s - %dms - ns=%s, %s@%s->%s/%s, connect_id=%d, mysql_connect_id=%d, transaction=%t|%v. err:%s",
			SQLExecStatusIgnore, 0, se.namespace, se.user, se.clientAddr, "", se.db, se.session.c.GetConnectionID(), 0, se.isInTransaction(), sql, "ignore lock tables")
		return nil, nil
	case *ast.RollbackStmt:
		return nil, se.handleRollback(stmt)
	case *ast.SavepointStmt:
		return nil, se.handleSavepoint(stmt)
	case *ast.UseStmt:
		return nil, se.handleUseDB(stmt.DBName)
	default:
		return nil, fmt.Errorf("cannot handle sql without plan, ns: %s, sql: %s", se.namespace, sql)
	}
}

func (se *SessionExecutor) handleUseDB(dbName string) error {
	if len(dbName) == 0 {
		return fmt.Errorf("must have database, the length of dbName is zero")
	}

	if se.GetNamespace().IsAllowedDB(dbName) {
		se.db = dbName
		return nil
	}

	return mysql.NewDefaultError(mysql.ErrNoDB)
}

func (se *SessionExecutor) getPlan(reqCtx *util.RequestContext, ns *Namespace, db string, sql string, checkHint bool) (plan.Plan, error) {
	p, isUnshardPlan := se.preBuildUnshardPlan(reqCtx, db, sql)
	if isUnshardPlan {
		return p, nil
	}
	n, err := se.Parse(sql)
	if err != nil {
		// 如果是注释的情况，则忽略
		if reqCtx.GetStmtType() == parser.StmtComment {
			se.manager.statistics.generalLogger.Warn("%s - %dms - ns=%s, %s@%s->%s/%s, connect_id=%d, mysql_connect_id=%d, transaction=%t|%v. err:%s",
				SQLExecStatusIgnore, 0, se.namespace, se.user, se.clientAddr, "", se.db, se.session.c.GetConnectionID(), 0, se.isInTransaction(), sql, "ignore syntax error")
			return plan.CreateIgnorePlan(), nil
		}
		return nil, fmt.Errorf("parse sql error, sql: %s, err: %v", sql, err)
	}

	var hintPlan plan.Plan
	if checkHint {
		//TODO: 获取 token 没有处理 `/* !mycat:sql=` hint，所以需要在这里处理下
		_, comments := extractPrefixCommentsAndRewrite(sql, se.session.proxy.ServerVersionCompareStatus)
		hintPlan, err = checkMyCatHintPlan(reqCtx, se, db, comments)
		// get MyCat hint plan error,will only log
		if err != nil {
			log.Notice("check MyCat hint plan err:%s", err)
		}
	}

	p, err = plan.BuildPlan(n, ns.GetPhysicalDBs(), db, sql, ns.GetRouter(), ns.GetGrayRouter(), ns.GetSequences(), hintPlan)
	if err != nil {
		return nil, fmt.Errorf("build plan error: %v", err)
	}

	return p, nil
}

// preBuildUnshardPlan pre-build unshard plan by shard rules or tokens
func (se *SessionExecutor) preBuildUnshardPlan(reqCtx *util.RequestContext, db string, sql string) (plan.Plan, bool) {
	rt := se.GetNamespace().GetRouter()
	grt := se.GetNamespace().GetGrayRouter()
	phyDBs := se.GetNamespace().GetPhysicalDBs()

	tokens := parser.Tokenize(sql)
	if len(tokens) == 0 {
		return nil, false
	}

	// to be used to check master hint
	reqCtx.SetTokens(tokens)

	// StmtComment not in UnshardPlan
	if reqCtx.GetStmtType() == parser.StmtComment {
		return nil, false
	}

	// select last_insert_id() not in UnshardPlan 使用长度判断，进一步降低命中的概率
	if len(tokens) > 1 && len(tokens[1]) > 13 && len(tokens[1]) < 17 {
		// select last_insert_id(); select last_insert_id (); select last_insert_id ( );select last_insert_id( );
		// select last_insert_id() as last_id; select last_insert_id ()  as last_id; select last_insert_id ( )  as last_id; select last_insert_id( )  as last_id;
		if compressSQL := strings.Join(tokens, ""); util.HasUpperPrefix(compressSQL, lastInsetIdMark) {
			return nil, false
		}
	}

	// preCheck unshard sql
	// 1. no shard rules return unshard plan directly
	if len(rt.GetAllRules()) == 0 && len(grt.GetAllRules()) == 0 {
		p, err := plan.PreCreateUnshardPlan(sql, phyDBs, db)
		if err == nil {
			return p, true
		}
		// if err occur, will further check sql
		log.Notice("pre create unshard plan with no sharding rules,will further check sql, ns:%s, sql: %s, err: %v", se.GetNamespace().GetName(), sql, err)
	}

	// 2. check sql, if all tables in sql are unshard, return unshard plan
	ruleDB := db
	isUnshardPlan := true
	tokenId, ok := mysql.ParseTokenMap[strings.ToLower(tokens[0])]
	if !ok {
		return nil, false
	}

	// TODO: deal with more sql type and optimize
	switch tokenId {
	case mysql.TkIdSelect, mysql.TkIdDelete:
		ruleDB, isUnshardPlan = plan.CheckUnshardBase(tokenId, tokens, rt, grt, db)
	case mysql.TkIdReplace, mysql.TkIdInsert:
		ruleDB, isUnshardPlan = plan.CheckUnshardInsert(tokens, rt, db)
	case mysql.TkIdUpdate:
		ruleDB, isUnshardPlan = plan.CheckUnshardUpdate(tokens, rt, db)
	default:
		return nil, false
	}

	if isUnshardPlan {
		// check databases and tables in sql
		p, err := plan.PreCreateUnshardPlan(sql, phyDBs, ruleDB)
		if err == nil {
			return p, true
		}
	}

	return nil, false
}

func (se *SessionExecutor) handleSet(reqCtx *util.RequestContext, sql string, stmt *ast.SetStmt) (*mysql.Result, error) {
	for _, v := range stmt.Variables {
		if err := se.handleSetVariable(sql, v); err != nil {
			return nil, err
		}
	}

	return nil, nil
}

func (se *SessionExecutor) handleSetVariable(sql string, v *ast.VariableAssignment) error {
	if v.IsGlobal {
		return fmt.Errorf("does not support set variable in global scope")
	}
	name := strings.ToLower(v.Name)
	switch name {
	case "character_set_results", "character_set_client", "character_set_connection":
		charset := getVariableExprResult(v.Value)
		if charset == "null" { // character_set_results允许设置成null, character_set_client和character_set_connection不允许
			return nil
		}
		if charset == mysql.KeywordDefault {
			se.charset = se.GetNamespace().GetDefaultCharset()
			se.collation = se.GetNamespace().GetDefaultCollationID()
			return nil
		}

		if err := se.sessionVariables.Set(name, charset); err != nil {
			return err
		}

		return nil
	case "group_concat_max_len":
		concatMaxLen := getVariableExprResult(v.Value)
		return se.setIntSessionVariable(mysql.GroupConcatMaxLen, concatMaxLen)

	case "autocommit":
		value := getVariableExprResult(v.Value)
		switch value {
		case mysql.KeywordDefault, "on", "1":
			return se.handleSetAutoCommit(true) // default set autocommit = 1
		case "off", "0":
			return se.handleSetAutoCommit(false)
		default:
			return mysql.NewDefaultError(mysql.ErrWrongValueForVar, name, value)
		}
	case "setnames": // SetNAMES represents SET NAMES 'xxx' COLLATE 'xxx'
		charset := getVariableExprResult(v.Value)
		if charset == mysql.KeywordDefault {
			charset = se.GetNamespace().GetDefaultCharset()
		}

		var collationID mysql.CollationID
		// if SET NAMES 'xxx' COLLATE DEFAULT, the parser treats it like SET NAMES 'xxx', and the ExtendValue is nil
		if v.ExtendValue != nil {
			collationName := getVariableExprResult(v.ExtendValue)
			cid, ok := mysql.CollationNames[collationName]
			if !ok {
				return mysql.NewDefaultError(mysql.ErrUnknownCharacterSet, charset)
			}
			toCharset, ok := mysql.CollationNameToCharset[collationName]
			if !ok {
				return mysql.NewDefaultError(mysql.ErrUnknownCharacterSet, charset)
			}
			if toCharset != charset { // collation与charset不匹配
				return mysql.NewDefaultError(mysql.ErrUnknownCharacterSet, charset)
			}
			collationID = cid
		} else {
			// if only set charset but not set collation, the collation is set to charset default collation implicitly.
			cid, ok := mysql.CharsetIds[charset]
			if !ok {
				return mysql.NewDefaultError(mysql.ErrUnknownCharacterSet, charset)
			}
			collationID = cid
		}

		se.charset = charset
		se.collation = collationID
		return nil
	case "sql_mode":
		sqlMode := getSqlModeExprResult(v.Value)
		return se.setStringSessionVariable(mysql.SQLModeStr, sqlMode)
	case "sql_safe_updates":
		value := getVariableExprResult(v.Value)
		onOffValue, err := getOnOffVariable(value)
		if err != nil {
			return mysql.NewDefaultError(mysql.ErrWrongValueForVar, name, value)
		}
		return se.setIntSessionVariable(mysql.SQLSafeUpdates, onOffValue)
	case "time_zone":
		value := getVariableExprResult(v.Value)
		return se.setStringSessionVariable(mysql.TimeZone, value)
	case "max_allowed_packet":
		return mysql.NewDefaultError(mysql.ErrVariableIsReadonly, "SESSION", mysql.MaxAllowedPacket, "GLOBAL")

		// do nothing
	case "wait_timeout", "interactive_timeout", "net_write_timeout", "net_read_timeout":
		return nil
	case "sql_select_limit":
		value := getVariableExprResult(v.Value)
		return se.setIntSessionVariable(mysql.SQLSelectLimit, value)
	case "transaction":
		return fmt.Errorf("does not support set transaction in gaea")
	case "tx_read_only", "transaction_read_only":
		//set session transaction read only; set session transaction read write ...
		value := getVariableExprResult(v.Value)
		onOffValue, err := getOnOffVariable(value)
		if err != nil {
			return mysql.NewDefaultError(mysql.ErrWrongValueForVar, name, value)
		}

		// mysql 8.0.3 not support tx_read_only
		if name == "tx_read_only" && !se.session.proxy.ServerVersionCompareStatus.LessThanMySQLVersion803 {
			return se.setIntSessionVariable("transaction_read_only", onOffValue)
		}

		return se.setIntSessionVariable(name, onOffValue)
	case gaeaGeneralLogVariable:
		value := getVariableExprResult(v.Value)
		onOffValue, err := getOnOffVariable(value)
		if err != nil {
			return mysql.NewDefaultError(mysql.ErrWrongValueForVar, name, value)
		}
		return se.setGeneralLogVariable(onOffValue)
	default:
		// 从命名空间获取允许用户配置的会话变量
		allowedVariables := se.GetNamespace().GetAllowedSessionVariables()
		// 如果变量名在命名空间中，则进行类型检查
		if variableType, ok := allowedVariables[name]; ok {
			switch variableType {
			case "int":
				value := getVariableExprResult(v.Value)
				return se.setIntSessionVariable(name, value)
			case "string":
				value := getVariableExprResult(v.Value)
				return se.setStringSessionVariable(name, value)
			case "bool":
				value := getVariableExprResult(v.Value)
				onOffValue, err := getOnOffVariable(value)
				if err != nil {
					return mysql.NewDefaultError(mysql.ErrWrongValueForVar, name, value)
				}
				return se.setIntSessionVariable(name, onOffValue)
			default:
				value := getVariableExprResult(v.Value)
				log.Warn("Unsupported session variable type for variable name: %s\n variable value %s\n", name, value)
			}
		}

		// unsupported variables will be ignored and logged to avoid user confusion
		// TODO: refactor sql exec time log
		se.manager.statistics.generalLogger.Warn("%s - %dms - ns=%s, %s@%s->%s/%s, connect_id=%d, mysql_connect_id=%d, transaction=%t|%v. err:%s",
			SQLExecStatusIgnore, 0, se.namespace, se.user, se.clientAddr, "", se.db, se.session.c.GetConnectionID(), 0, se.isInTransaction(),
			sql, fmt.Sprintf("variable(%s) not supported", name))
		return nil
	}
}

func (se *SessionExecutor) handleSetAutoCommit(autocommit bool) (err error) {
	se.txLock.Lock()
	defer se.txLock.Unlock()

	if autocommit {
		se.status |= mysql.ServerStatusAutocommit
		if se.status&mysql.ServerStatusInTrans > 0 {
			se.status &= ^mysql.ServerStatusInTrans
		}
		for _, pc := range se.txConns {
			if e := pc.SetAutoCommit(1); e != nil {
				err = fmt.Errorf("set autocommit error, %v", e)
			}
			pc.Recycle()
		}
		se.txConns = make(map[string]backend.PooledConnect)
		return
	}

	se.status &= ^mysql.ServerStatusAutocommit
	return
}

func (se *SessionExecutor) handleStmtPrepare(sql string) (*Stmt, error) {
	log.Debug("namespace: %s use prepare, sql: %s", se.GetNamespace().GetName(), sql)

	stmt := new(Stmt)

	sql = strings.TrimRight(sql, ";")
	stmt.sql = sql

	paramCount, offsets, sqlItems, err := CalcParams(stmt.sql)
	if err != nil {
		log.Warn("prepare calc params failed, namespace: %s, sql: %s", se.GetNamespace().GetName(), sql)
		return nil, err
	}

	stmt.sqlItems = sqlItems
	stmt.paramCount = paramCount
	stmt.offsets = offsets
	stmt.id = se.stmtID
	stmt.columnCount = 0
	se.stmtID++

	stmt.ResetParams()
	se.stmts[stmt.id] = stmt

	return stmt, nil
}

func (se *SessionExecutor) handleStmtClose(data []byte) error {
	if len(data) < 4 {
		return nil
	}

	id := binary.LittleEndian.Uint32(data[0:4])

	delete(se.stmts, id)

	return nil
}

func (se *SessionExecutor) handleFieldList(data []byte) ([]*mysql.Field, error) {
	index := bytes.IndexByte(data, 0x00)
	table := string(data[0:index])
	wildcard := string(data[index+1:])

	sliceName := se.GetNamespace().GetRouter().GetRule(se.GetDatabase(), table).GetSlice(0)

	pc, err := se.getBackendConn(sliceName, se.GetNamespace().IsRWSplit(se.user))
	if err != nil {
		return nil, err
	}
	defer se.recycleBackendConn(pc)
	phyDB, err := se.GetNamespace().GetDefaultPhyDB(se.GetDatabase())
	if err != nil {
		return nil, err
	}

	if err = initBackendConn(pc, phyDB, se.GetCharset(), se.GetCollationID(), se.GetVariables()); err != nil {
		return nil, err
	}

	fs, err := pc.FieldList(table, wildcard)
	if err != nil {
		return nil, err
	}

	return fs, nil
}

func (se *SessionExecutor) handleKeepSessionPing() (err error) {
	for _, ksConn := range se.ksConns {
		if err = ksConn.PingWithTimeout(backend.GetConnTimeout); err != nil {
			ksConn.Close()
			break
		}
	}

	if err != nil {
		for _, ksConn := range se.ksConns {
			if se.isInTransaction() {
				ksConn.Close()
			}
			ksConn.Recycle()
		}
		return mysql.ErrBadConn
	}

	return nil
}

// getMyCatHintSQL get SQL from MyCat hints
func getMyCatHintSQL(hints string) string {
	tmp := strings.Split(hints, mycatHint+"sql=")
	if len(tmp) < 2 {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(tmp[1], "*/"))
}
