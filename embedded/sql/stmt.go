/*
Copyright 2021 CodeNotary, Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sql

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/codenotary/immudb/embedded/store"
)

const (
	catalogDatabasePrefix = "CTL.DATABASE." // (key=CTL.DATABASE.{dbID}, value={dbNAME})
	catalogTablePrefix    = "CTL.TABLE."    // (key=CTL.TABLE.{dbID}{tableID}, value={tableNAME})
	catalogColumnPrefix   = "CTL.COLUMN."   // (key=CTL.COLUMN.{dbID}{tableID}{colID}{colTYPE}, value={(auto_incremental | nullable){maxLen}{colNAME}})
	catalogIndexPrefix    = "CTL.INDEX."    // (key=CTL.INDEX.{dbID}{tableID}{indexID}, value={unique {colID1}(ASC|DESC)...{colIDN}(ASC|DESC)})
	PIndexPrefix          = "P."            // (key=P.{dbID}{tableID}{0}({pkVal}{padding}{pkValLen})+, value={DELETED count (colID valLen val)+})
	SIndexPrefix          = "S."            // (key=S.{dbID}{tableID}{indexID}({val}{padding}{valLen})+({pkVal}{padding}{pkValLen})+, value={DELETED})
	UIndexPrefix          = "U."            // (key=U.{dbID}{tableID}{indexID}({val}{padding}{valLen})+, value={DELETED ({pkVal}{padding}{pkValLen})+})
)

const PKIndexID = uint32(0)

const (
	nullableFlag      byte = 1 << iota
	autoIncrementFlag byte = 1 << iota
)

type SQLValueType = string

const (
	IntegerType   SQLValueType = "INTEGER"
	BooleanType   SQLValueType = "BOOLEAN"
	VarcharType   SQLValueType = "VARCHAR"
	BLOBType      SQLValueType = "BLOB"
	TimestampType SQLValueType = "TIMESTAMP"
	AnyType       SQLValueType = "ANY"
)

type AggregateFn = string

const (
	COUNT AggregateFn = "COUNT"
	SUM   AggregateFn = "SUM"
	MAX   AggregateFn = "MAX"
	MIN   AggregateFn = "MIN"
	AVG   AggregateFn = "AVG"
)

type CmpOperator = int

const (
	EQ CmpOperator = iota
	NE
	LT
	LE
	GT
	GE
)

type LogicOperator = int

const (
	AND LogicOperator = iota
	OR
)

type NumOperator = int

const (
	ADDOP NumOperator = iota
	SUBSOP
	DIVOP
	MULTOP
)

type JoinType = int

const (
	InnerJoin JoinType = iota
	LeftJoin
	RightJoin
)

var deletedOrMustNotExist store.KVConstraint = func(key, currValue []byte, err error) error {
	if err == store.ErrKeyNotFound {
		return nil
	}
	if err != nil {
		return err
	}

	// ignore deleted
	if len(currValue) > 0 && currValue[0] == 1 {
		return nil
	}

	return store.ErrKeyAlreadyExists
}

type TxSummary struct {
	db *Database

	ces []*store.KV
	des []*store.KV

	updatedRows     int
	lastInsertedPKs map[string]int64
}

func newTxSummary(db *Database) *TxSummary {
	return &TxSummary{
		db:              db,
		lastInsertedPKs: make(map[string]int64),
	}
}

func (s *TxSummary) add(summary *TxSummary) error {
	if summary == nil {
		return ErrIllegalArguments
	}

	s.db = summary.db

	s.updatedRows += summary.updatedRows

	s.ces = append(s.ces, summary.ces...)
	s.des = append(s.des, summary.des...)

	for t, pk := range summary.lastInsertedPKs {
		s.lastInsertedPKs[t] = pk
	}

	return nil
}

type SQLStmt interface {
	compileUsing(e *Engine, implicitDB *Database, params map[string]interface{}) (summary *TxSummary, err error)
	inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error
}

type TxStmt struct {
	stmts []SQLStmt
}

func (stmt *TxStmt) inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error {
	for _, stmt := range stmt.stmts {
		err := stmt.inferParameters(e, implicitDB, params)
		if err != nil {
			return err
		}
	}

	return nil
}

func (stmt *TxStmt) compileUsing(e *Engine, implicitDB *Database, params map[string]interface{}) (summary *TxSummary, err error) {
	summary = newTxSummary(implicitDB)

	for _, stmt := range stmt.stmts {
		stmtSummary, err := stmt.compileUsing(e, summary.db, params)
		if err != nil {
			return nil, err
		}

		err = summary.add(stmtSummary)
		if err != nil {
			return nil, err
		}
	}

	return summary, nil
}

type CreateDatabaseStmt struct {
	DB string
}

func (stmt *CreateDatabaseStmt) inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error {
	return nil
}

func (stmt *CreateDatabaseStmt) compileUsing(e *Engine, implicitDB *Database, params map[string]interface{}) (summary *TxSummary, err error) {
	id := uint32(len(e.catalog.dbsByID) + 1)

	db, err := e.catalog.newDatabase(id, stmt.DB)
	if err != nil {
		return nil, err
	}

	summary = newTxSummary(db)

	kv := &store.KV{
		Key:   e.mapKey(catalogDatabasePrefix, EncodeID(db.id)),
		Value: []byte(stmt.DB),
	}

	summary.ces = append(summary.ces, kv)

	return summary, nil
}

type UseDatabaseStmt struct {
	DB string
}

func (stmt *UseDatabaseStmt) inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error {
	return nil
}

func (stmt *UseDatabaseStmt) compileUsing(e *Engine, implicitDB *Database, params map[string]interface{}) (summary *TxSummary, err error) {
	db, err := e.catalog.GetDatabaseByName(stmt.DB)
	if err != nil {
		return nil, err
	}

	return newTxSummary(db), nil
}

type UseSnapshotStmt struct {
	sinceTx  uint64
	asBefore uint64
}

func (stmt *UseSnapshotStmt) inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error {
	return nil
}

func (stmt *UseSnapshotStmt) compileUsing(e *Engine, implicitDB *Database, params map[string]interface{}) (summary *TxSummary, err error) {
	return nil, ErrNoSupported
}

type CreateTableStmt struct {
	table       string
	ifNotExists bool
	colsSpec    []*ColSpec
	pkColNames  []string
}

func (stmt *CreateTableStmt) inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error {
	return nil
}

func (stmt *CreateTableStmt) compileUsing(e *Engine, implicitDB *Database, params map[string]interface{}) (summary *TxSummary, err error) {
	if implicitDB == nil {
		return nil, ErrNoDatabaseSelected
	}

	summary = newTxSummary(implicitDB)

	if stmt.ifNotExists && implicitDB.ExistTable(stmt.table) {
		return summary, nil
	}

	table, err := implicitDB.newTable(stmt.table, stmt.colsSpec)
	if err != nil {
		return nil, err
	}

	createIndexStmt := &CreateIndexStmt{unique: true, table: table.name, cols: stmt.pkColNames}
	indexSummary, err := createIndexStmt.compileUsing(e, implicitDB, params)
	if err != nil {
		return nil, err
	}

	err = summary.add(indexSummary)
	if err != nil {
		return nil, err
	}

	for _, col := range table.Cols() {
		//{auto_incremental | nullable}{maxLen}{colNAME})
		v := make([]byte, 1+4+len(col.colName))

		if col.autoIncrement {
			if len(table.primaryIndex.cols) > 1 || col.id != table.primaryIndex.cols[0].id {
				return nil, ErrLimitedAutoIncrement
			}

			v[0] = v[0] | autoIncrementFlag
		}

		if col.notNull {
			v[0] = v[0] | nullableFlag
		}

		binary.BigEndian.PutUint32(v[1:], uint32(col.MaxLen()))

		copy(v[5:], []byte(col.Name()))

		ce := &store.KV{
			Key:   e.mapKey(catalogColumnPrefix, EncodeID(implicitDB.id), EncodeID(table.id), EncodeID(col.id), []byte(col.colType)),
			Value: v,
		}
		summary.ces = append(summary.ces, ce)
	}

	te := &store.KV{
		Key:   e.mapKey(catalogTablePrefix, EncodeID(implicitDB.id), EncodeID(table.id)),
		Value: []byte(table.name),
	}
	summary.ces = append(summary.ces, te)

	return summary, nil
}

type ColSpec struct {
	colName       string
	colType       SQLValueType
	maxLen        int
	autoIncrement bool
	notNull       bool
}

type CreateIndexStmt struct {
	unique bool
	table  string
	cols   []string
}

func (stmt *CreateIndexStmt) inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error {
	return nil
}

func (stmt *CreateIndexStmt) compileUsing(e *Engine, implicitDB *Database, params map[string]interface{}) (summary *TxSummary, err error) {
	if len(stmt.cols) < 1 {
		return nil, ErrIllegalArguments
	}

	if len(stmt.cols) > MaxNumberOfColumnsInIndex {
		return nil, ErrMaxNumberOfColumnsInIndexExceeded
	}

	if implicitDB == nil {
		return nil, ErrNoDatabaseSelected
	}

	summary = newTxSummary(implicitDB)

	table, err := implicitDB.GetTableByName(stmt.table)
	if err != nil {
		return nil, err
	}

	// check table is empty
	{
		lastTxID, _ := e.dataStore.Alh()
		err = e.dataStore.WaitForIndexingUpto(lastTxID, nil)
		if err != nil {
			return nil, err
		}

		pkPrefix := e.mapKey(PIndexPrefix, EncodeID(table.db.id), EncodeID(table.id), EncodeID(PKIndexID))
		existKey, err := e.dataStore.ExistKeyWith(pkPrefix, pkPrefix, false)
		if err != nil {
			return nil, err
		}
		if existKey {
			return nil, ErrLimitedIndexCreation
		}
	}

	colIDs := make([]uint32, len(stmt.cols))

	for i, colName := range stmt.cols {
		col, err := table.GetColumnByName(colName)
		if err != nil {
			return nil, err
		}

		if variableSized(col.colType) && (col.MaxLen() == 0 || col.MaxLen() > maxKeyLen) {
			return nil, ErrLimitedKeyType
		}

		colIDs[i] = col.id
	}

	index, err := table.newIndex(stmt.unique, colIDs)
	if err != nil {
		return nil, err
	}

	// v={unique {colID1}(ASC|DESC)...{colIDN}(ASC|DESC)}
	// TODO: currently only ASC order is supported
	colSpecLen := EncIDLen + 1

	encodedValues := make([]byte, 1+len(index.cols)*colSpecLen)

	if index.IsUnique() {
		encodedValues[0] = 1
	}

	for i, col := range index.cols {
		copy(encodedValues[1+i*colSpecLen:], EncodeID(col.id))
	}

	te := &store.KV{
		Key:   e.mapKey(catalogIndexPrefix, EncodeID(table.db.id), EncodeID(table.id), EncodeID(index.id)),
		Value: encodedValues,
	}
	summary.ces = append(summary.ces, te)

	return summary, nil
}

type AddColumnStmt struct {
	table   string
	colSpec *ColSpec
}

func (stmt *AddColumnStmt) inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error {
	return nil
}

func (stmt *AddColumnStmt) compileUsing(e *Engine, implicitDB *Database, params map[string]interface{}) (summary *TxSummary, err error) {
	return nil, ErrNoSupported
}

type UpsertIntoStmt struct {
	isInsert bool
	tableRef *tableRef
	cols     []string
	rows     []*RowSpec
}

type RowSpec struct {
	Values []ValueExp
}

func (stmt *UpsertIntoStmt) inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error {
	for _, row := range stmt.rows {
		if len(stmt.cols) != len(row.Values) {
			return ErrIllegalArguments
		}

		for i, val := range row.Values {
			table, err := stmt.tableRef.referencedTable(e, implicitDB)
			if err != nil {
				return err
			}

			col, err := table.GetColumnByName(stmt.cols[i])
			if err != nil {
				return err
			}

			err = val.requiresType(col.colType, make(map[string]*ColDescriptor), params, implicitDB.name, table.name)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (stmt *UpsertIntoStmt) validate(table *Table) (map[uint32]int, error) {
	selPosByColID := make(map[uint32]int, len(stmt.cols))

	for i, c := range stmt.cols {
		col, err := table.GetColumnByName(c)
		if err != nil {
			return nil, err
		}

		_, duplicated := selPosByColID[col.id]
		if duplicated {
			return nil, ErrDuplicatedColumn
		}

		selPosByColID[col.id] = i
	}

	return selPosByColID, nil
}

func (stmt *UpsertIntoStmt) compileUsing(e *Engine, implicitDB *Database, params map[string]interface{}) (summary *TxSummary, err error) {
	if implicitDB == nil {
		return nil, ErrNoDatabaseSelected
	}

	summary = newTxSummary(implicitDB)

	table, err := stmt.tableRef.referencedTable(e, implicitDB)
	if err != nil {
		return nil, err
	}

	selPosByColID, err := stmt.validate(table)
	if err != nil {
		return nil, err
	}

	for _, row := range stmt.rows {
		if len(row.Values) != len(stmt.cols) {
			return nil, ErrInvalidNumberOfValues
		}

		valuesByColID := make(map[uint32]TypedValue)

		for colID, col := range table.colsByID {
			colPos, specified := selPosByColID[colID]
			if !specified {
				// TODO: Default values
				if col.notNull {
					return nil, ErrNotNullableColumnCannotBeNull
				}
				continue
			}

			if stmt.isInsert && col.autoIncrement {
				return nil, ErrNoValueForAutoIncrementalColumn
			}

			cVal := row.Values[colPos]

			val, err := cVal.substitute(params)
			if err != nil {
				return nil, err
			}

			rval, err := val.reduce(e.catalog, nil, implicitDB.name, table.name)
			if err != nil {
				return nil, err
			}

			_, isNull := rval.(*NullValue)
			if isNull {
				if col.notNull {
					return nil, ErrNotNullableColumnCannotBeNull
				}

				continue
			}

			valuesByColID[colID] = rval
		}

		// inject auto-incremental pk value
		if stmt.isInsert && table.autoIncrementPK {
			table.maxPK++
			e.catalog.mutated = true // TODO: implement transactional in-memory catalog

			pkCol := table.primaryIndex.cols[0]

			valuesByColID[pkCol.id] = &Number{val: table.maxPK}

			summary.lastInsertedPKs[table.name] = table.maxPK
		}

		valbuf := bytes.Buffer{}

		for _, col := range table.primaryIndex.cols {
			rval, notNull := valuesByColID[col.id]
			if !notNull {
				return nil, ErrPKCanNotBeNull
			}

			encVal, err := EncodeAsKey(rval.Value(), col.colType, col.MaxLen())
			if err != nil {
				return nil, fmt.Errorf(
					"error setting value for column %s.%s.%s: %w",
					table.db.Name(),
					table.Name(),
					col.Name(),
					err,
				)
			}

			if len(encVal) > maxKeyLen {
				return nil, ErrMaxKeyLengthExceeded
			}

			_, err = valbuf.Write(encVal)
			if err != nil {
				return nil, err
			}
		}

		pkEncVals := valbuf.Bytes()

		var reusableIndexEntries map[uint32]struct{}

		if !stmt.isInsert && len(table.indexes) > 1 {
			currPKRow, err := e.fetchPKRow(table, valuesByColID)
			if err != nil && err != ErrNoMoreRows {
				return nil, err
			}

			if err == nil {
				currValuesByColID := make(map[uint32]TypedValue, len(currPKRow.Values))

				for _, col := range table.cols {
					encSel := EncodeSelector("", table.db.name, table.name, col.colName)
					currValuesByColID[col.id] = currPKRow.Values[encSel]
				}

				reusableIndexEntries, err = e.deleteIndexEntriesFor(pkEncVals, currValuesByColID, valuesByColID, table, summary)
				if err != nil {
					return nil, err
				}
			}
		}

		valbuf = bytes.Buffer{}

		b := make([]byte, EncLenLen)
		binary.BigEndian.PutUint32(b, uint32(len(valuesByColID)))

		_, err = valbuf.Write(b)
		if err != nil {
			return nil, err
		}

		for _, col := range table.cols {
			rval, notNull := valuesByColID[col.id]
			if !notNull {
				continue
			}

			b := make([]byte, EncIDLen)
			binary.BigEndian.PutUint32(b, uint32(col.id))

			_, err = valbuf.Write(b)
			if err != nil {
				return nil, err
			}

			encVal, err := EncodeValue(rval.Value(), col.colType, col.MaxLen())
			if err != nil {
				return nil, fmt.Errorf(
					"error setting value for column %s.%s.%s: %w",
					table.db.Name(),
					table.Name(),
					col.Name(),
					err,
				)
			}

			_, err = valbuf.Write(encVal)
			if err != nil {
				return nil, err
			}
		}

		// create primary index entry
		mkey := e.mapKey(PIndexPrefix, EncodeID(table.db.id), EncodeID(table.id), EncodeID(table.primaryIndex.id), pkEncVals)

		var constraint store.KVConstraint

		if stmt.isInsert && !table.autoIncrementPK {
			constraint = deletedOrMustNotExist
		}

		if !stmt.isInsert && table.autoIncrementPK {
			constraint = store.MustExist
		}

		pke := &store.KV{
			Key:        mkey,
			Value:      append([]byte{0}, valbuf.Bytes()...),
			Constraint: constraint,
		}
		summary.des = append(summary.des, pke)

		// create entries for secondary indexes
		for _, index := range table.indexes {
			if index.IsPrimary() {
				continue
			}

			if reusableIndexEntries != nil {
				_, reusable := reusableIndexEntries[index.id]
				if reusable {
					continue
				}
			}

			var prefix string
			var encodedValues [][]byte
			var val []byte

			if index.IsUnique() {
				prefix = UIndexPrefix
				encodedValues = make([][]byte, 3+len(index.cols))
				val = append([]byte{0}, pkEncVals...)
			} else {
				prefix = SIndexPrefix
				encodedValues = make([][]byte, 4+len(index.cols))
				encodedValues[len(encodedValues)-1] = pkEncVals
				val = []byte{0} // unset deletion flag
			}

			encodedValues[0] = EncodeID(table.db.id)
			encodedValues[1] = EncodeID(table.id)
			encodedValues[2] = EncodeID(index.id)

			for i, col := range index.cols {
				rval, notNull := valuesByColID[col.id]
				if !notNull {
					rval = &NullValue{t: col.colType}
				}

				encVal, err := EncodeAsKey(rval.Value(), col.colType, col.MaxLen())
				if err != nil {
					return nil, err
				}

				if len(encVal) > maxKeyLen {
					return nil, ErrMaxKeyLengthExceeded
				}

				encodedValues[i+3] = encVal
			}

			var constraint store.KVConstraint

			if index.IsUnique() {
				constraint = deletedOrMustNotExist
			}

			ie := &store.KV{
				Key:        e.mapKey(prefix, encodedValues...),
				Value:      val,
				Constraint: constraint,
			}

			summary.des = append(summary.des, ie)
		}

		summary.updatedRows++
	}

	return summary, nil
}

func (e *Engine) fetchPKRow(table *Table, valuesByColID map[uint32]TypedValue) (*Row, error) {
	pkRanges := make(map[uint32]*typedValueRange, len(table.primaryIndex.cols))

	for _, pkCol := range table.primaryIndex.cols {
		pkVal := valuesByColID[pkCol.id]

		pkRanges[pkCol.id] = &typedValueRange{
			lRange: &typedValueSemiRange{val: pkVal, inclusive: true},
			hRange: &typedValueSemiRange{val: pkVal, inclusive: true},
		}
	}

	scanSpecs := &ScanSpecs{
		index:         table.primaryIndex,
		rangesByColID: pkRanges,
	}

	lastTxID, _ := e.dataStore.Alh()
	err := e.dataStore.WaitForIndexingUpto(lastTxID, nil)
	if err != nil {
		return nil, err
	}

	snapshot := e.dataStore.CurrentSnapshot()
	defer func() {
		snapshot.Close()
	}()

	r, err := e.newRawRowReader(snapshot, table, 0, table.name, scanSpecs)
	if err != nil {
		return nil, err
	}

	defer func() {
		r.Close()
	}()

	return r.Read()
}

// deleteIndexEntriesFor mark previous index entries as deleted
func (e *Engine) deleteIndexEntriesFor(
	pkEncVals []byte,
	currValuesByColID, newValuesByColID map[uint32]TypedValue,
	table *Table,
	summary *TxSummary) (reusableIndexEntries map[uint32]struct{}, err error) {

	reusableIndexEntries = make(map[uint32]struct{})

	for _, index := range table.indexes {
		if index.IsPrimary() {
			continue
		}

		var prefix string
		var encodedValues [][]byte
		var val []byte

		if index.IsUnique() {
			prefix = UIndexPrefix
			encodedValues = make([][]byte, 3+len(index.cols))
			val = append([]byte{1}, pkEncVals...)
		} else {
			prefix = SIndexPrefix
			encodedValues = make([][]byte, 4+len(index.cols))
			encodedValues[len(encodedValues)-1] = pkEncVals
			val = []byte{1} // set deletion flag
		}

		encodedValues[0] = EncodeID(table.db.id)
		encodedValues[1] = EncodeID(table.id)
		encodedValues[2] = EncodeID(index.id)

		// existent index entry is deleted only if it differs from existent one
		sameIndexKey := true

		for i, col := range index.cols {
			currVal, isNotNull := currValuesByColID[col.id]
			if !isNotNull {
				currVal = &NullValue{t: col.colType}
			}

			newVal, isNotNull := newValuesByColID[col.id]
			if !isNotNull {
				newVal = &NullValue{t: col.colType}
			}

			r, err := currVal.Compare(newVal)
			if err != nil {
				return nil, err
			}

			sameIndexKey = sameIndexKey && r == 0

			encVal, _ := EncodeAsKey(currVal.Value(), col.colType, col.MaxLen())

			encodedValues[i+3] = encVal
		}

		// mark existent index entry as deleted
		if sameIndexKey {
			reusableIndexEntries[index.id] = struct{}{}
		} else {
			ie := &store.KV{
				Key:   e.mapKey(prefix, encodedValues...),
				Value: val,
			}

			summary.des = append(summary.des, ie)
		}
	}

	return reusableIndexEntries, nil
}

type ValueExp interface {
	inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error)
	requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error
	substitute(params map[string]interface{}) (ValueExp, error)
	reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error)
	reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp
	isConstant() bool
	selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error
}

type typedValueRange struct {
	lRange *typedValueSemiRange
	hRange *typedValueSemiRange
}

type typedValueSemiRange struct {
	val       TypedValue
	inclusive bool
}

func (r *typedValueRange) unitary() bool {
	// TODO: this simplified implementation doesn't cover all unitary cases e.g. 3<=v<4
	if r.lRange == nil || r.hRange == nil {
		return false
	}

	res, _ := r.lRange.val.Compare(r.hRange.val)
	return res == 0 && r.lRange.inclusive && r.hRange.inclusive
}

func (r *typedValueRange) refineWith(refiningRange *typedValueRange) error {
	if r.lRange == nil {
		r.lRange = refiningRange.lRange
	} else if r.lRange != nil && refiningRange.lRange != nil {
		maxRange, err := maxSemiRange(r.lRange, refiningRange.lRange)
		if err != nil {
			return err
		}
		r.lRange = maxRange
	}

	if r.hRange == nil {
		r.hRange = refiningRange.hRange
	} else if r.hRange != nil && refiningRange.hRange != nil {
		minRange, err := minSemiRange(r.hRange, refiningRange.hRange)
		if err != nil {
			return err
		}
		r.hRange = minRange
	}

	return nil
}

func (r *typedValueRange) extendWith(extendingRange *typedValueRange) error {
	if r.lRange == nil || extendingRange.lRange == nil {
		r.lRange = nil
	} else {
		minRange, err := minSemiRange(r.lRange, extendingRange.lRange)
		if err != nil {
			return err
		}
		r.lRange = minRange
	}

	if r.hRange == nil || extendingRange.hRange == nil {
		r.hRange = nil
	} else {
		maxRange, err := maxSemiRange(r.hRange, extendingRange.hRange)
		if err != nil {
			return err
		}
		r.hRange = maxRange
	}

	return nil
}

func maxSemiRange(or1, or2 *typedValueSemiRange) (*typedValueSemiRange, error) {
	r, err := or1.val.Compare(or2.val)
	if err != nil {
		return nil, err
	}

	maxVal := or1.val
	if r < 0 {
		maxVal = or2.val
	}

	return &typedValueSemiRange{
		val:       maxVal,
		inclusive: or1.inclusive && or2.inclusive,
	}, nil
}

func minSemiRange(or1, or2 *typedValueSemiRange) (*typedValueSemiRange, error) {
	r, err := or1.val.Compare(or2.val)
	if err != nil {
		return nil, err
	}

	minVal := or1.val
	if r > 0 {
		minVal = or2.val
	}

	return &typedValueSemiRange{
		val:       minVal,
		inclusive: or1.inclusive || or2.inclusive,
	}, nil
}

type TypedValue interface {
	ValueExp
	Type() SQLValueType
	Value() interface{}
	Compare(val TypedValue) (int, error)
}

type NullValue struct {
	t SQLValueType
}

func (n *NullValue) Type() SQLValueType {
	return n.t
}

func (n *NullValue) Value() interface{} {
	return nil
}

func (n *NullValue) Compare(val TypedValue) (int, error) {
	if n.t != AnyType && val.Type() != AnyType && n.t != val.Type() {
		return 0, ErrNotComparableValues
	}

	if val.Value() == nil {
		return 0, nil
	}

	return -1, nil
}

func (v *NullValue) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	return v.t, nil
}

func (v *NullValue) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if v.t == t {
		return nil
	}

	if v.t != AnyType {
		return ErrInvalidTypes
	}

	v.t = t

	return nil
}

func (v *NullValue) substitute(params map[string]interface{}) (ValueExp, error) {
	return v, nil
}

func (v *NullValue) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	return v, nil
}

func (v *NullValue) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return v
}

func (v *NullValue) isConstant() bool {
	return true
}

func (v *NullValue) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

type Number struct {
	val int64
}

func (v *Number) Type() SQLValueType {
	return IntegerType
}

func (v *Number) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	return IntegerType, nil
}

func (v *Number) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if t != IntegerType {
		return ErrInvalidTypes
	}

	return nil
}

func (v *Number) substitute(params map[string]interface{}) (ValueExp, error) {
	return v, nil
}

func (v *Number) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	return v, nil
}

func (v *Number) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return v
}

func (v *Number) isConstant() bool {
	return true
}

func (v *Number) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

func (v *Number) Value() interface{} {
	return v.val
}

func (v *Number) Compare(val TypedValue) (int, error) {
	_, isNull := val.(*NullValue)
	if isNull {
		return 1, nil
	}

	if val.Type() != IntegerType {
		return 0, ErrNotComparableValues
	}

	rval := val.Value().(int64)

	if v.val == rval {
		return 0, nil
	}

	if v.val > rval {
		return 1, nil
	}

	return -1, nil
}

type Varchar struct {
	val string
}

func (v *Varchar) Type() SQLValueType {
	return VarcharType
}

func (v *Varchar) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	return VarcharType, nil
}

func (v *Varchar) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if t != VarcharType {
		return ErrInvalidTypes
	}

	return nil
}

func (v *Varchar) substitute(params map[string]interface{}) (ValueExp, error) {
	return v, nil
}

func (v *Varchar) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	return v, nil
}

func (v *Varchar) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return v
}

func (v *Varchar) isConstant() bool {
	return true
}

func (v *Varchar) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

func (v *Varchar) Value() interface{} {
	return v.val
}

func (v *Varchar) Compare(val TypedValue) (int, error) {
	_, isNull := val.(*NullValue)
	if isNull {
		return 1, nil
	}

	if val.Type() != VarcharType {
		return 0, ErrNotComparableValues
	}

	rval := val.Value().(string)

	return bytes.Compare([]byte(v.val), []byte(rval)), nil
}

type Bool struct {
	val bool
}

func (v *Bool) Type() SQLValueType {
	return BooleanType
}

func (v *Bool) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	return BooleanType, nil
}

func (v *Bool) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if t != BooleanType {
		return ErrInvalidTypes
	}

	return nil
}

func (v *Bool) substitute(params map[string]interface{}) (ValueExp, error) {
	return v, nil
}

func (v *Bool) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	return v, nil
}

func (v *Bool) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return v
}

func (v *Bool) isConstant() bool {
	return true
}

func (v *Bool) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

func (v *Bool) Value() interface{} {
	return v.val
}

func (v *Bool) Compare(val TypedValue) (int, error) {
	_, isNull := val.(*NullValue)
	if isNull {
		return 1, nil
	}

	if val.Type() != BooleanType {
		return 0, ErrNotComparableValues
	}

	rval := val.Value().(bool)

	if v.val == rval {
		return 0, nil
	}

	if v.val {
		return 1, nil
	}

	return -1, nil
}

type Blob struct {
	val []byte
}

func (v *Blob) Type() SQLValueType {
	return BLOBType
}

func (v *Blob) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	return BLOBType, nil
}

func (v *Blob) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if t != BLOBType {
		return ErrInvalidTypes
	}

	return nil
}

func (v *Blob) substitute(params map[string]interface{}) (ValueExp, error) {
	return v, nil
}

func (v *Blob) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	return v, nil
}

func (v *Blob) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return v
}

func (v *Blob) isConstant() bool {
	return true
}

func (v *Blob) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

func (v *Blob) Value() interface{} {
	return v.val
}

func (v *Blob) Compare(val TypedValue) (int, error) {
	_, isNull := val.(*NullValue)
	if isNull {
		return 1, nil
	}

	if val.Type() != BLOBType {
		return 0, ErrNotComparableValues
	}

	rval := val.Value().([]byte)

	return bytes.Compare(v.val, rval), nil
}

type SysFn struct {
	fn string
}

func (v *SysFn) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	if strings.ToUpper(v.fn) == "NOW" {
		return IntegerType, nil
	}

	return AnyType, ErrIllegalArguments
}

func (v *SysFn) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if strings.ToUpper(v.fn) == "NOW" {
		if t != IntegerType {
			return ErrInvalidTypes
		}

		return nil
	}

	return ErrIllegalArguments
}

func (v *SysFn) substitute(params map[string]interface{}) (ValueExp, error) {
	return v, nil
}

func (v *SysFn) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	if strings.ToUpper(v.fn) == "NOW" {
		return &Number{val: time.Now().UnixNano()}, nil
	}

	return nil, errors.New("not yet supported")
}

func (v *SysFn) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return v
}

func (v *SysFn) isConstant() bool {
	return false
}

func (v *SysFn) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

type Param struct {
	id  string
	pos int
}

func (v *Param) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	t, ok := params[v.id]
	if !ok {
		params[v.id] = AnyType
		return AnyType, nil
	}

	return t, nil
}

func (v *Param) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	currT, ok := params[v.id]
	if ok && currT != t && currT != AnyType {
		return ErrInferredMultipleTypes
	}

	params[v.id] = t

	return nil
}

func (p *Param) substitute(params map[string]interface{}) (ValueExp, error) {
	val, ok := params[p.id]
	if !ok {
		return nil, ErrMissingParameter
	}

	if val == nil {
		return &NullValue{t: AnyType}, nil
	}

	switch v := val.(type) {
	case bool:
		{
			return &Bool{val: v}, nil
		}
	case string:
		{
			return &Varchar{val: v}, nil
		}
	case int:
		{
			return &Number{val: int64(v)}, nil
		}
	case uint:
		{
			return &Number{val: int64(v)}, nil
		}
	case uint64:
		{
			return &Number{val: int64(v)}, nil
		}
	case int64:
		{
			return &Number{val: v}, nil
		}
	case []byte:
		{
			return &Blob{val: v}, nil
		}
	}

	return nil, ErrUnsupportedParameter
}

func (p *Param) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	return nil, ErrUnexpected
}

func (p *Param) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return p
}

func (p *Param) isConstant() bool {
	return true
}

func (v *Param) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

type Comparison int

const (
	EqualTo Comparison = iota
	LowerThan
	LowerOrEqualTo
	GreaterThan
	GreaterOrEqualTo
)

type DataSource interface {
	inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error
	Resolve(e *Engine, snap *store.Snapshot, implicitDB *Database, params map[string]interface{}, ScanSpecs *ScanSpecs) (RowReader, error)
	Alias() string
}

type SelectStmt struct {
	distinct  bool
	selectors []Selector
	ds        DataSource
	indexOn   []string
	joins     []*JoinSpec
	where     ValueExp
	groupBy   []*ColSelector
	having    ValueExp
	limit     int
	orderBy   []*OrdCol
	as        string
}

type ScanSpecs struct {
	index          *Index
	rangesByColID  map[uint32]*typedValueRange
	includeDeleted bool
	descOrder      bool
}

func (stmt *SelectStmt) Limit() int {
	return stmt.limit
}

func (stmt *SelectStmt) inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error {
	_, err := stmt.compileUsing(e, implicitDB, nil)
	if err != nil {
		return err
	}

	snapshot, err := e.getSnapshot()
	if err != nil {
		return err
	}

	// TODO (jeroiraz) may be optimized so to resolve the query statement just once
	rowReader, err := stmt.Resolve(e, snapshot, implicitDB, nil, nil)
	if err != nil {
		return err
	}
	defer rowReader.Close()

	return rowReader.InferParameters(params)
}

func (stmt *SelectStmt) compileUsing(e *Engine, implicitDB *Database, params map[string]interface{}) (summary *TxSummary, err error) {
	if implicitDB == nil {
		return nil, ErrNoDatabaseSelected
	}

	if stmt.groupBy == nil && stmt.having != nil {
		return nil, ErrHavingClauseRequiresGroupClause
	}

	if len(stmt.groupBy) > 1 {
		return nil, ErrLimitedGroupBy
	}

	if len(stmt.orderBy) > 1 {
		return nil, ErrLimitedOrderBy
	}

	if len(stmt.orderBy) > 0 {
		tableRef, ok := stmt.ds.(*tableRef)
		if !ok {
			return nil, ErrLimitedOrderBy
		}

		table, err := tableRef.referencedTable(e, implicitDB)
		if err != nil {
			return nil, err
		}

		col, err := table.GetColumnByName(stmt.orderBy[0].sel.col)
		if err != nil {
			return nil, err
		}

		_, indexed := table.indexesByColID[col.id]
		if !indexed {
			return nil, ErrLimitedOrderBy
		}
	}

	return newTxSummary(implicitDB), nil
}

func (stmt *SelectStmt) Resolve(e *Engine, snap *store.Snapshot, implicitDB *Database, params map[string]interface{}, _ *ScanSpecs) (rowReader RowReader, err error) {
	scanSpecs, err := stmt.genScanSpecs(e, snap, implicitDB, params)
	if err != nil {
		return nil, err
	}

	rowReader, err = stmt.ds.Resolve(e, snap, implicitDB, params, scanSpecs)
	if err != nil {
		return nil, err
	}

	if stmt.joins != nil {
		rowReader, err = e.newJointRowReader(implicitDB, snap, params, rowReader, stmt.joins)
		if err != nil {
			return nil, err
		}
	}

	if stmt.where != nil {
		rowReader, err = e.newConditionalRowReader(rowReader, stmt.where, params)
		if err != nil {
			return nil, err
		}
	}

	containsAggregations := false
	for _, sel := range stmt.selectors {
		_, containsAggregations = sel.(*AggColSelector)
		if containsAggregations {
			break
		}
	}

	if containsAggregations {
		var groupBy []*ColSelector
		if stmt.groupBy != nil {
			groupBy = stmt.groupBy
		}

		rowReader, err = e.newGroupedRowReader(rowReader, stmt.selectors, groupBy)
		if err != nil {
			return nil, err
		}

		if stmt.having != nil {
			rowReader, err = e.newConditionalRowReader(rowReader, stmt.having, params)
			if err != nil {
				return nil, err
			}
		}
	}

	rowReader, err = e.newProjectedRowReader(rowReader, stmt.as, stmt.selectors)
	if err != nil {
		return nil, err
	}

	if stmt.distinct {
		rowReader, err = e.newDistinctRowReader(rowReader)
		if err != nil {
			return nil, err
		}
	}

	if stmt.limit > 0 {
		return e.newLimitRowReader(rowReader, stmt.limit)
	}

	return rowReader, nil
}

func (stmt *SelectStmt) Alias() string {
	if stmt.as == "" {
		return stmt.ds.Alias()
	}

	return stmt.as
}

func (stmt *SelectStmt) genScanSpecs(e *Engine, snap *store.Snapshot, implicitDB *Database, params map[string]interface{}) (*ScanSpecs, error) {
	tableRef, isTableRef := stmt.ds.(*tableRef)
	if !isTableRef {
		return nil, nil
	}

	table, err := tableRef.referencedTable(e, implicitDB)
	if err != nil {
		return nil, err
	}

	rangesByColID := make(map[uint32]*typedValueRange)
	if stmt.where != nil {
		err = stmt.where.selectorRanges(table, tableRef.Alias(), params, rangesByColID)
		if err != nil {
			return nil, err
		}
	}

	var preferredIndex *Index

	if len(stmt.indexOn) > 0 {
		cols := make([]*Column, len(stmt.indexOn))

		for i, colName := range stmt.indexOn {
			col, err := table.GetColumnByName(colName)
			if err != nil {
				return nil, err
			}

			cols[i] = col
		}

		index, ok := table.indexes[indexKeyFrom(cols)]
		if !ok {
			return nil, ErrNoAvailableIndex
		}

		preferredIndex = index
	}

	var sortingIndex *Index
	var descOrder bool

	if stmt.orderBy == nil {
		if preferredIndex == nil {
			sortingIndex = table.primaryIndex
		} else {
			sortingIndex = preferredIndex
		}
	}

	if len(stmt.orderBy) > 0 {
		col, err := table.GetColumnByName(stmt.orderBy[0].sel.col)
		if err != nil {
			return nil, err
		}

		for _, idx := range table.indexesByColID[col.id] {
			if idx.sortableUsing(col.id, rangesByColID) {
				if preferredIndex == nil || idx.id == preferredIndex.id {
					sortingIndex = idx
					break
				}
			}
		}

		descOrder = stmt.orderBy[0].descOrder
	}

	if sortingIndex == nil {
		return nil, ErrNoAvailableIndex
	}

	return &ScanSpecs{
		index:         sortingIndex,
		rangesByColID: rangesByColID,
		descOrder:     descOrder,
	}, nil
}

type tableRef struct {
	db       string
	table    string
	asBefore uint64
	as       string
}

func (stmt *tableRef) referencedTable(e *Engine, implicitDB *Database) (*Table, error) {
	var db *Database

	if stmt.db != "" {
		rdb, err := e.catalog.GetDatabaseByName(stmt.db)
		if err != nil {
			return nil, err
		}

		db = rdb
	}

	if db == nil {
		if implicitDB == nil {
			return nil, ErrNoDatabaseSelected
		}

		db = implicitDB
	}

	table, err := db.GetTableByName(stmt.table)
	if err != nil {
		return nil, err
	}

	return table, nil
}

func (stmt *tableRef) inferParameters(e *Engine, implicitDB *Database, params map[string]SQLValueType) error {
	return nil
}

func (stmt *tableRef) Resolve(e *Engine, snap *store.Snapshot, implicitDB *Database, params map[string]interface{}, scanSpecs *ScanSpecs) (RowReader, error) {
	if e == nil || snap == nil {
		return nil, ErrIllegalArguments
	}

	table, err := stmt.referencedTable(e, implicitDB)
	if err != nil {
		return nil, err
	}

	asBefore := stmt.asBefore
	if asBefore == 0 {
		asBefore = e.snapAsBeforeTx
	}

	return e.newRawRowReader(snap, table, asBefore, stmt.as, scanSpecs)
}

func (stmt *tableRef) Alias() string {
	if stmt.as == "" {
		return stmt.table
	}
	return stmt.as
}

type JoinSpec struct {
	joinType JoinType
	ds       DataSource
	cond     ValueExp
	indexOn  []string
}

type OrdCol struct {
	sel       *ColSelector
	descOrder bool
}

type Selector interface {
	ValueExp
	resolve(implicitDB, implicitTable string) (aggFn, db, table, col string)
	alias() string
	setAlias(alias string)
}

type ColSelector struct {
	db    string
	table string
	col   string
	as    string
}

func (sel *ColSelector) resolve(implicitDB, implicitTable string) (aggFn, db, table, col string) {
	db = implicitDB
	if sel.db != "" {
		db = sel.db
	}

	table = implicitTable
	if sel.table != "" {
		table = sel.table
	}

	return "", db, table, sel.col
}

func (sel *ColSelector) alias() string {
	if sel.as == "" {
		return sel.col
	}

	return sel.as
}

func (sel *ColSelector) setAlias(alias string) {
	sel.as = alias
}

func (sel *ColSelector) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	_, db, table, col := sel.resolve(implicitDB, implicitTable)
	encSel := EncodeSelector("", db, table, col)

	desc, ok := cols[encSel]
	if !ok {
		return AnyType, ErrInvalidColumn
	}

	return desc.Type, nil
}

func (sel *ColSelector) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	_, db, table, col := sel.resolve(implicitDB, implicitTable)
	encSel := EncodeSelector("", db, table, col)

	desc, ok := cols[encSel]
	if !ok {
		return ErrInvalidColumn
	}

	if desc.Type != t {
		return ErrInvalidTypes
	}

	return nil
}

func (sel *ColSelector) substitute(params map[string]interface{}) (ValueExp, error) {
	return sel, nil
}

func (sel *ColSelector) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	if row == nil {
		return nil, ErrInvalidValue
	}

	aggFn, db, table, col := sel.resolve(implicitDB, implicitTable)

	v, ok := row.Values[EncodeSelector(aggFn, db, table, col)]
	if !ok {
		return nil, ErrColumnDoesNotExist
	}

	return v, nil
}

func (sel *ColSelector) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	aggFn, db, table, col := sel.resolve(implicitDB, implicitTable)

	v, ok := row.Values[EncodeSelector(aggFn, db, table, col)]
	if !ok {
		return sel
	}

	return v
}

func (sel *ColSelector) isConstant() bool {
	return false
}

func (sel *ColSelector) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

type AggColSelector struct {
	aggFn AggregateFn
	db    string
	table string
	col   string
	as    string
}

func EncodeSelector(aggFn, db, table, col string) string {
	return aggFn + "(" + db + "." + table + "." + col + ")"
}

func (sel *AggColSelector) resolve(implicitDB, implicitTable string) (aggFn, db, table, col string) {
	db = implicitDB
	if sel.db != "" {
		db = sel.db
	}

	table = implicitTable
	if sel.table != "" {
		table = sel.table
	}

	return sel.aggFn, db, table, sel.col
}

func (sel *AggColSelector) alias() string {
	return sel.as
}

func (sel *AggColSelector) setAlias(alias string) {
	sel.as = alias
}

func (sel *AggColSelector) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	if sel.aggFn == COUNT {
		return IntegerType, nil
	}

	colSelector := &ColSelector{db: sel.db, table: sel.table, col: sel.col}

	if sel.aggFn == SUM || sel.aggFn == AVG {
		err := colSelector.requiresType(IntegerType, cols, params, implicitDB, implicitTable)
		if err != nil {
			return AnyType, ErrInvalidTypes
		}

		return IntegerType, nil
	}

	return colSelector.inferType(cols, params, implicitDB, implicitTable)
}

func (sel *AggColSelector) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if sel.aggFn == COUNT {
		if t != IntegerType {
			return ErrInvalidTypes
		}
		return nil
	}

	colSelector := &ColSelector{db: sel.db, table: sel.table, col: sel.col}

	if sel.aggFn == SUM || sel.aggFn == AVG {
		return colSelector.requiresType(IntegerType, cols, params, implicitDB, implicitTable)
	}

	return colSelector.requiresType(t, cols, params, implicitDB, implicitTable)
}

func (sel *AggColSelector) substitute(params map[string]interface{}) (ValueExp, error) {
	return sel, nil
}

func (sel *AggColSelector) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	v, ok := row.Values[EncodeSelector(sel.resolve(implicitDB, implicitTable))]
	if !ok {
		return nil, ErrColumnDoesNotExist
	}
	return v, nil
}

func (sel *AggColSelector) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return sel
}

func (sel *AggColSelector) isConstant() bool {
	return false
}

func (sel *AggColSelector) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

type NumExp struct {
	op          NumOperator
	left, right ValueExp
}

func (bexp *NumExp) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	err := bexp.left.requiresType(IntegerType, cols, params, implicitDB, implicitTable)
	if err != nil {
		return AnyType, err
	}

	err = bexp.right.requiresType(IntegerType, cols, params, implicitDB, implicitTable)
	if err != nil {
		return AnyType, err
	}

	return IntegerType, nil
}

func (bexp *NumExp) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if t != IntegerType {
		return ErrInvalidTypes
	}

	err := bexp.left.requiresType(IntegerType, cols, params, implicitDB, implicitTable)
	if err != nil {
		return err
	}

	err = bexp.right.requiresType(IntegerType, cols, params, implicitDB, implicitTable)
	if err != nil {
		return err
	}

	return nil
}

func (bexp *NumExp) substitute(params map[string]interface{}) (ValueExp, error) {
	rlexp, err := bexp.left.substitute(params)
	if err != nil {
		return nil, err
	}

	rrexp, err := bexp.right.substitute(params)
	if err != nil {
		return nil, err
	}

	bexp.left = rlexp
	bexp.right = rrexp

	return bexp, nil
}

func (bexp *NumExp) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	vl, err := bexp.left.reduce(catalog, row, implicitDB, implicitTable)
	if err != nil {
		return nil, err
	}

	vr, err := bexp.right.reduce(catalog, row, implicitDB, implicitTable)
	if err != nil {
		return nil, err
	}

	nl, isNumber := vl.Value().(int64)
	if !isNumber {
		return nil, ErrInvalidValue
	}

	nr, isNumber := vr.Value().(int64)
	if !isNumber {
		return nil, ErrInvalidValue
	}

	switch bexp.op {
	case ADDOP:
		{
			return &Number{val: nl + nr}, nil
		}
	case SUBSOP:
		{
			return &Number{val: nl - nr}, nil
		}
	case DIVOP:
		{
			if nr == 0 {
				return nil, ErrDivisionByZero
			}

			return &Number{val: nl / nr}, nil
		}
	case MULTOP:
		{
			return &Number{val: nl * nr}, nil
		}
	}

	return nil, ErrUnexpected
}

func (bexp *NumExp) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return &NumExp{
		op:    bexp.op,
		left:  bexp.left.reduceSelectors(row, implicitDB, implicitTable),
		right: bexp.right.reduceSelectors(row, implicitDB, implicitTable),
	}
}

func (bexp *NumExp) isConstant() bool {
	return bexp.left.isConstant() && bexp.right.isConstant()
}

func (bexp *NumExp) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

type NotBoolExp struct {
	exp ValueExp
}

func (bexp *NotBoolExp) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	err := bexp.exp.requiresType(BooleanType, cols, params, implicitDB, implicitTable)
	if err != nil {
		return AnyType, err
	}

	return BooleanType, nil
}

func (bexp *NotBoolExp) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if t != BooleanType {
		return ErrInvalidTypes
	}

	return bexp.exp.requiresType(BooleanType, cols, params, implicitDB, implicitTable)
}

func (bexp *NotBoolExp) substitute(params map[string]interface{}) (ValueExp, error) {
	rexp, err := bexp.exp.substitute(params)
	if err != nil {
		return nil, err
	}

	bexp.exp = rexp

	return bexp, nil
}

func (bexp *NotBoolExp) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	v, err := bexp.exp.reduce(catalog, row, implicitDB, implicitTable)
	if err != nil {
		return nil, err
	}

	r, isBool := v.Value().(bool)
	if !isBool {
		return nil, ErrInvalidCondition
	}

	return &Bool{val: !r}, nil
}

func (bexp *NotBoolExp) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return &NotBoolExp{
		exp: bexp.exp.reduceSelectors(row, implicitDB, implicitTable),
	}
}

func (bexp *NotBoolExp) isConstant() bool {
	return bexp.exp.isConstant()
}

func (bexp *NotBoolExp) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

type LikeBoolExp struct {
	sel     Selector
	pattern string
}

func (bexp *LikeBoolExp) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	return BooleanType, nil
}

func (bexp *LikeBoolExp) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if t != BooleanType {
		return ErrInvalidTypes
	}

	return nil
}

func (bexp *LikeBoolExp) substitute(params map[string]interface{}) (ValueExp, error) {
	return bexp, nil
}

func (bexp *LikeBoolExp) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	v, ok := row.Values[EncodeSelector(bexp.sel.resolve(implicitDB, implicitTable))]
	if !ok {
		return nil, ErrColumnDoesNotExist
	}

	if v.Type() != VarcharType {
		return nil, ErrInvalidColumn
	}

	matched, err := regexp.MatchString(bexp.pattern, v.Value().(string))
	if err != nil {
		return nil, err
	}

	return &Bool{val: matched}, nil
}

func (bexp *LikeBoolExp) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return bexp
}

func (bexp *LikeBoolExp) isConstant() bool {
	return false
}

func (bexp *LikeBoolExp) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}

type CmpBoolExp struct {
	op          CmpOperator
	left, right ValueExp
}

func (bexp *CmpBoolExp) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	tleft, err := bexp.left.inferType(cols, params, implicitDB, implicitTable)
	if err != nil {
		return AnyType, err
	}

	tright, err := bexp.right.inferType(cols, params, implicitDB, implicitTable)
	if err != nil {
		return AnyType, err
	}

	// unification step

	if tleft == tright {
		return BooleanType, nil
	}

	if tleft != AnyType && tright != AnyType {
		return AnyType, ErrInvalidTypes
	}

	if tleft == AnyType {
		err = bexp.left.requiresType(tright, cols, params, implicitDB, implicitTable)
		if err != nil {
			return AnyType, err
		}
	}

	if tright == AnyType {
		err = bexp.right.requiresType(tleft, cols, params, implicitDB, implicitTable)
		if err != nil {
			return AnyType, err
		}
	}

	return BooleanType, nil
}

func (bexp *CmpBoolExp) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if t != BooleanType {
		return ErrInvalidTypes
	}

	_, err := bexp.inferType(cols, params, implicitDB, implicitTable)

	return err
}

func (bexp *CmpBoolExp) substitute(params map[string]interface{}) (ValueExp, error) {
	rlexp, err := bexp.left.substitute(params)
	if err != nil {
		return nil, err
	}

	rrexp, err := bexp.right.substitute(params)
	if err != nil {
		return nil, err
	}

	bexp.left = rlexp
	bexp.right = rrexp

	return bexp, nil
}

func (bexp *CmpBoolExp) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	vl, err := bexp.left.reduce(catalog, row, implicitDB, implicitTable)
	if err != nil {
		return nil, err
	}

	vr, err := bexp.right.reduce(catalog, row, implicitDB, implicitTable)
	if err != nil {
		return nil, err
	}

	r, err := vl.Compare(vr)
	if err != nil {
		return nil, err
	}

	return &Bool{val: cmpSatisfiesOp(r, bexp.op)}, nil
}

func (bexp *CmpBoolExp) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return &CmpBoolExp{
		op:    bexp.op,
		left:  bexp.left.reduceSelectors(row, implicitDB, implicitTable),
		right: bexp.right.reduceSelectors(row, implicitDB, implicitTable),
	}
}

func (bexp *CmpBoolExp) isConstant() bool {
	return bexp.left.isConstant() && bexp.right.isConstant()
}

func (bexp *CmpBoolExp) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	matchingFunc := func(left, right ValueExp) (*ColSelector, ValueExp, bool) {
		s, isSel := bexp.left.(*ColSelector)
		if isSel && bexp.right.isConstant() {
			return s, right, true
		}
		return nil, nil, false
	}

	sel, c, ok := matchingFunc(bexp.left, bexp.right)
	if !ok {
		sel, c, ok = matchingFunc(bexp.right, bexp.left)
	}

	if !ok {
		return nil
	}

	aggFn, db, t, col := sel.resolve(table.db.name, table.name)
	if aggFn != "" || db != table.db.name || t != asTable {
		return nil
	}

	column, err := table.GetColumnByName(col)
	if err != nil {
		return err
	}

	val, err := c.substitute(params)
	if err == ErrMissingParameter {
		// TODO: not supported when parameters are not provided during query resolution
		return nil
	}
	if err != nil {
		return err
	}

	rval, err := val.reduce(nil, nil, table.db.name, table.name)
	if err != nil {
		return err
	}

	return updateRangeFor(column.id, rval, bexp.op, rangesByColID)
}

func updateRangeFor(colID uint32, val TypedValue, cmp CmpOperator, rangesByColID map[uint32]*typedValueRange) error {
	currRange, ranged := rangesByColID[colID]
	var newRange *typedValueRange

	switch cmp {
	case EQ:
		{
			newRange = &typedValueRange{
				lRange: &typedValueSemiRange{
					val:       val,
					inclusive: true,
				},
				hRange: &typedValueSemiRange{
					val:       val,
					inclusive: true,
				},
			}
		}
	case LT:
		{
			newRange = &typedValueRange{
				hRange: &typedValueSemiRange{
					val: val,
				},
			}
		}
	case LE:
		{
			newRange = &typedValueRange{
				hRange: &typedValueSemiRange{
					val:       val,
					inclusive: true,
				},
			}
		}
	case GT:
		{
			newRange = &typedValueRange{
				lRange: &typedValueSemiRange{
					val: val,
				},
			}
		}
	case GE:
		{
			newRange = &typedValueRange{
				lRange: &typedValueSemiRange{
					val:       val,
					inclusive: true,
				},
			}
		}
	case NE:
		{
			return nil
		}
	}

	if !ranged {
		rangesByColID[colID] = newRange
		return nil
	}

	return currRange.refineWith(newRange)
}

func cmpSatisfiesOp(cmp int, op CmpOperator) bool {
	switch cmp {
	case 0:
		{
			return op == EQ || op == LE || op == GE
		}
	case -1:
		{
			return op == NE || op == LT || op == LE
		}
	case 1:
		{
			return op == NE || op == GT || op == GE
		}
	}
	return false
}

type BinBoolExp struct {
	op          LogicOperator
	left, right ValueExp
}

func (bexp *BinBoolExp) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	err := bexp.left.requiresType(BooleanType, cols, params, implicitDB, implicitTable)
	if err != nil {
		return AnyType, err
	}

	err = bexp.right.requiresType(BooleanType, cols, params, implicitDB, implicitTable)
	if err != nil {
		return AnyType, err
	}

	return BooleanType, nil
}

func (bexp *BinBoolExp) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	if t != BooleanType {
		return ErrInvalidTypes
	}

	err := bexp.left.requiresType(BooleanType, cols, params, implicitDB, implicitTable)
	if err != nil {
		return err
	}

	err = bexp.right.requiresType(BooleanType, cols, params, implicitDB, implicitTable)
	if err != nil {
		return err
	}

	return nil
}

func (bexp *BinBoolExp) substitute(params map[string]interface{}) (ValueExp, error) {
	rlexp, err := bexp.left.substitute(params)
	if err != nil {
		return nil, err
	}

	rrexp, err := bexp.right.substitute(params)
	if err != nil {
		return nil, err
	}

	bexp.left = rlexp
	bexp.right = rrexp

	return bexp, nil
}

func (bexp *BinBoolExp) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	vl, err := bexp.left.reduce(catalog, row, implicitDB, implicitTable)
	if err != nil {
		return nil, err
	}

	vr, err := bexp.right.reduce(catalog, row, implicitDB, implicitTable)
	if err != nil {
		return nil, err
	}

	bl, isBool := vl.(*Bool)
	if !isBool {
		return nil, ErrInvalidValue
	}

	br, isBool := vr.(*Bool)
	if !isBool {
		return nil, ErrInvalidValue
	}

	switch bexp.op {
	case AND:
		{
			return &Bool{val: bl.val && br.val}, nil
		}
	case OR:
		{
			return &Bool{val: bl.val || br.val}, nil
		}
	}

	return nil, ErrUnexpected
}

func (bexp *BinBoolExp) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return &BinBoolExp{
		op:    bexp.op,
		left:  bexp.left.reduceSelectors(row, implicitDB, implicitTable),
		right: bexp.right.reduceSelectors(row, implicitDB, implicitTable),
	}
}

func (bexp *BinBoolExp) isConstant() bool {
	return bexp.left.isConstant() && bexp.right.isConstant()
}

func (bexp *BinBoolExp) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	if bexp.op == AND {
		err := bexp.left.selectorRanges(table, asTable, params, rangesByColID)
		if err != nil {
			return err
		}

		return bexp.right.selectorRanges(table, asTable, params, rangesByColID)
	}

	lRanges := make(map[uint32]*typedValueRange)
	rRanges := make(map[uint32]*typedValueRange)

	err := bexp.left.selectorRanges(table, asTable, params, lRanges)
	if err != nil {
		return err
	}

	err = bexp.right.selectorRanges(table, asTable, params, rRanges)
	if err != nil {
		return err
	}

	for colID, lr := range lRanges {
		rr, ok := rRanges[colID]
		if !ok {
			continue
		}

		err = lr.extendWith(rr)
		if err != nil {
			return err
		}

		rangesByColID[colID] = lr
	}

	return nil
}

type ExistsBoolExp struct {
	q *SelectStmt
}

func (bexp *ExistsBoolExp) inferType(cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) (SQLValueType, error) {
	return AnyType, errors.New("not yet supported")
}

func (bexp *ExistsBoolExp) requiresType(t SQLValueType, cols map[string]*ColDescriptor, params map[string]SQLValueType, implicitDB, implicitTable string) error {
	return errors.New("not yet supported")
}

func (bexp *ExistsBoolExp) substitute(params map[string]interface{}) (ValueExp, error) {
	return bexp, nil
}

func (bexp *ExistsBoolExp) reduce(catalog *Catalog, row *Row, implicitDB, implicitTable string) (TypedValue, error) {
	return nil, errors.New("not yet supported")
}

func (bexp *ExistsBoolExp) reduceSelectors(row *Row, implicitDB, implicitTable string) ValueExp {
	return bexp
}

func (bexp *ExistsBoolExp) isConstant() bool {
	return false
}

func (bexp *ExistsBoolExp) selectorRanges(table *Table, asTable string, params map[string]interface{}, rangesByColID map[uint32]*typedValueRange) error {
	return nil
}
