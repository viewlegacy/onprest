package agent

import (
	"errors"

	mssql "github.com/denisenkom/go-mssqldb"
	"github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
	"github.com/sijms/go-ora/v2/network"
)

const (
	errorConstraintViolation = "AGENT_CONSTRAINT_VIOLATION"
	errorOutcomeUnknown      = "AGENT_TRANSACTION_OUTCOME_UNKNOWN"
)

func classifyDBError(driver string, err error) string {
	if err == nil {
		return ""
	}
	switch driver {
	case "postgres":
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && len(pqErr.Code) >= 2 && string(pqErr.Code[:2]) == "23" {
			return errorConstraintViolation
		}
	case "mysql":
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) {
			if len(mysqlErr.SQLState) >= 2 && string(mysqlErr.SQLState[:2]) == "23" {
				return errorConstraintViolation
			}
			switch mysqlErr.Number {
			case 1062, 1451, 1452, 1048, 3819:
				return errorConstraintViolation
			}
		}
	case "sqlserver":
		var sqlErr mssql.Error
		if errors.As(err, &sqlErr) {
			switch sqlErr.Number {
			case 2627, 2601, 547, 515:
				return errorConstraintViolation
			}
		}
	case "oracle":
		var oracleErr *network.OracleError
		if errors.As(err, &oracleErr) {
			switch oracleErr.ErrCode {
			case 1, 2290, 2291, 2292, 1400:
				return errorConstraintViolation
			}
		}
	}
	return ""
}
