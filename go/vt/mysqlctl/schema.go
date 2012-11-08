// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mysqlctl

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"code.google.com/p/vitess/go/jscfg"
	"code.google.com/p/vitess/go/relog"
)

type TableDefinition struct {
	Name   string // the table name
	Schema string // the SQL to run to create the table
}

type SchemaDefinition struct {
	// ordered by TableDefinition.Name
	TableDefinitions []TableDefinition

	// the md5 of the concatenation of TableDefinition.Schema
	Version string
}

func (sd *SchemaDefinition) String() string {
	return jscfg.ToJson(sd)
}

func (sd *SchemaDefinition) generateSchemaVersion() {
	hasher := md5.New()
	for _, td := range sd.TableDefinitions {
		if _, err := hasher.Write([]byte(td.Schema)); err != nil {
			// extremely unlikely
			panic(err)
		}
	}
	sd.Version = hex.EncodeToString(hasher.Sum(nil))
}

// generates a report on what's different between two SchemaDefinition
func (left *SchemaDefinition) DiffSchema(leftName, rightName string, right *SchemaDefinition, result chan string) {
	leftIndex := 0
	rightIndex := 0
	for leftIndex < len(left.TableDefinitions) && rightIndex < len(right.TableDefinitions) {
		// extra table on the left side
		if left.TableDefinitions[leftIndex].Name < right.TableDefinitions[rightIndex].Name {
			result <- leftName + " has an extra table named " + left.TableDefinitions[leftIndex].Name
			leftIndex++
			continue
		}

		// extra table on the right side
		if left.TableDefinitions[leftIndex].Name > right.TableDefinitions[rightIndex].Name {
			result <- rightName + " has an extra table named " + right.TableDefinitions[rightIndex].Name
			rightIndex++
			continue
		}

		// same name, let's see content
		if left.TableDefinitions[leftIndex].Schema != right.TableDefinitions[rightIndex].Schema {
			result <- leftName + " and " + rightName + " disagree on schema for table " + left.TableDefinitions[leftIndex].Name
		}
		leftIndex++
		rightIndex++
	}

	for leftIndex < len(left.TableDefinitions) {
		result <- leftName + " has an extra table named " + left.TableDefinitions[leftIndex].Name
		leftIndex++
	}
	for rightIndex < len(right.TableDefinitions) {
		result <- rightName + " has an extra table named " + right.TableDefinitions[rightIndex].Name
		rightIndex++
	}
	return
}

func (left *SchemaDefinition) DiffSchemaToArray(leftName, rightName string, right *SchemaDefinition) (result []string) {
	schemaDiffs := make(chan string, 10)
	go func() {
		left.DiffSchema(leftName, rightName, right, schemaDiffs)
		close(schemaDiffs)
	}()
	result = make([]string, 0, 10)
	for msg := range schemaDiffs {
		result = append(result, msg)
	}
	return result
}

var autoIncr = regexp.MustCompile("auto_increment=\\d+")

// Return the schema for a database
func (mysqld *Mysqld) GetSchema(dbName string) (*SchemaDefinition, error) {
	rows, err := mysqld.fetchSuperQuery("SHOW TABLES IN " + dbName)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return &SchemaDefinition{}, nil
	}
	sd := &SchemaDefinition{TableDefinitions: make([]TableDefinition, len(rows))}
	for i, row := range rows {
		tableName := row[0].String()
		relog.Info("GetSchema(table: %v)", tableName)

		rows, fetchErr := mysqld.fetchSuperQuery("SHOW CREATE TABLE " + dbName + "." + tableName)
		if fetchErr != nil {
			return nil, fetchErr
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("empty create table statement for %v", tableName)
		}

		// Normalize & remove auto_increment because it changes on every insert
		// FIXME(alainjobart) find a way to share this with
		// vt/tabletserver/table_info.go:162
		norm1 := strings.ToLower(rows[0][1].String())
		norm2 := autoIncr.ReplaceAllLiteralString(norm1, "")

		sd.TableDefinitions[i].Name = tableName
		sd.TableDefinitions[i].Schema = norm2
	}

	sd.generateSchemaVersion()
	return sd, nil
}

type SchemaChange struct {
	Sql              string
	Force            bool
	AllowReplication bool
	BeforeSchema     *SchemaDefinition
	AfterSchema      *SchemaDefinition
}

type SchemaChangeResult struct {
	Error        string
	BeforeSchema *SchemaDefinition
	AfterSchema  *SchemaDefinition
}

func (scr *SchemaChangeResult) String() string {
	return jscfg.ToJson(scr)
}

func (mysqld *Mysqld) PreflightSchemaChange(dbName string, change string) (result *SchemaChangeResult) {
	result = &SchemaChangeResult{}

	// gather current schema on real database
	var err error
	result.BeforeSchema, err = mysqld.GetSchema(dbName)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// populate temporary database with it
	sql := "SET sql_log_bin = 0;\n"
	sql += "DROP DATABASE IF EXISTS _vt_preflight;\n"
	sql += "CREATE DATABASE _vt_preflight;\n"
	sql += "USE _vt_preflight;\n"
	for _, td := range result.BeforeSchema.TableDefinitions {
		sql += td.Schema + ";\n"
	}
	err = mysqld.ExecuteMysqlCommand(sql)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// apply schema change to the temporary database
	sql = "SET sql_log_bin = 0;\n"
	sql += "USE _vt_preflight;\n"
	sql += change
	err = mysqld.ExecuteMysqlCommand(sql)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// get the result
	result.AfterSchema, err = mysqld.GetSchema("_vt_preflight")
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// and clean up the extra database
	sql = "SET sql_log_bin = 0;\n"
	sql += "DROP DATABASE _vt_preflight;\n"
	err = mysqld.ExecuteMysqlCommand(sql)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	return result
}

func (mysqld *Mysqld) ApplySchemaChange(dbName string, change *SchemaChange) (result *SchemaChangeResult) {
	result = &SchemaChangeResult{}

	// check current schema matches
	var err error
	result.BeforeSchema, err = mysqld.GetSchema(dbName)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if change.BeforeSchema != nil {
		schemaDiffs := result.BeforeSchema.DiffSchemaToArray("actual", "expected", change.BeforeSchema)
		if len(schemaDiffs) > 0 {
			for _, msg := range schemaDiffs {
				relog.Warning("BeforeSchema differs: %v", msg)
			}

			// let's see if the schema was already applied
			if change.AfterSchema != nil {
				schemaDiffs = result.BeforeSchema.DiffSchemaToArray("actual", "expected", change.AfterSchema)
				if len(schemaDiffs) == 0 {
					// no diff between the schema we expect
					// after the change and the current
					// schema, we already applied it
					result.AfterSchema = result.BeforeSchema
					return result
				}
			}

			if change.Force {
				relog.Warning("BeforeSchema differs, applying anyway")
			} else {
				result.Error = "BeforeSchema differs"
				return result
			}
		}
	}

	sql := change.Sql
	if !change.AllowReplication {
		sql = "SET sql_log_bin = 0;\n" + sql
	}

	// add a 'use XXX' in front of the SQL
	sql = "USE " + dbName + ";\n" + sql

	// execute the schema change using an external mysql process
	// (to benefit from the extra commands in mysql cli)
	err = mysqld.ExecuteMysqlCommand(sql)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// populate AfterSchema
	result.AfterSchema, err = mysqld.GetSchema(dbName)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	// compare to the provided AfterSchema
	if change.AfterSchema != nil {
		schemaDiffs := result.AfterSchema.DiffSchemaToArray("actual", "expected", change.AfterSchema)
		if len(schemaDiffs) > 0 {
			for _, msg := range schemaDiffs {
				relog.Warning("AfterSchema differs: %v", msg)
			}
			if change.Force {
				relog.Warning("AfterSchema differs, not reporting error")
			} else {
				result.Error = "AfterSchema differs"
				return result
			}
		}
	}

	return result
}
