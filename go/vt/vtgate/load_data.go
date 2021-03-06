package vtgate

import (
	"bytes"
	"errors"
	"strings"

	querypb "vitess.io/vitess/go/vt/proto/query"
	"vitess.io/vitess/go/vt/sqlparser"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/vtgate/vindexes"
	"vitess.io/vitess/go/vt/vterrors"
)

// InsertFunc insert callback
type InsertFunc func(insert string) error

// NewLoadData create LoadData return pointer
func NewLoadData() *LoadData {
	return &LoadData{
		LoadDataInfo: &LoadDataInfo{
			maxRowsInBatch: *loadMaxRowsInBatch,
		},
	}
}

// LoadData include load_data status info and package info
type LoadData struct {
	LoadDataInfo *LoadDataInfo
}

// LoadDataInfo params
type LoadDataInfo struct {
	maxRowsInBatch int
	LinesInfo      *sqlparser.LinesClause
	FieldsInfo     *sqlparser.FieldsClause
	Columns        sqlparser.Columns
	Table          *sqlparser.TableName
}

// SetMaxRowsInBatch sets the max number of rows to insert in a batch.
func (l *LoadDataInfo) SetMaxRowsInBatch(limit int) {
	l.maxRowsInBatch = limit
}

// ParseLoadDataPram parse the load data statement
func (l *LoadDataInfo) ParseLoadDataPram(loadStmt *sqlparser.LoadDataStmt) {
	l.Columns = loadStmt.Columns
	l.Table = &loadStmt.Table
	l.FieldsInfo = loadStmt.FieldsInfo
	l.LinesInfo = loadStmt.LinesInfo

}

// seFieldString parse column by enclosed
func (l *LoadDataInfo) parseFieldString(column string, enclosed byte) (string, error) {
	if enclosed == ([]byte(" "))[0] || column == "" {
		return column, nil
	} else if strings.HasPrefix(column, string(enclosed)) && strings.HasSuffix(column, string(enclosed)) {
		start := len(string(enclosed)) - 1
		end := len(column) - len(string(enclosed)) + 1
		return column[start:end], nil
	}
	return column, nil
}

// getValidData returns prevData and curData that starts from starting symbol.
// If the data doesn't have starting symbol, prevData is nil and curData is curData[len(curData)-startingLen+1:].
// If curData size less than startingLen, curData is returned directly.
func (l *LoadDataInfo) getValidData(prevData, curData []byte) ([]byte, []byte) {
	startingLen := len(l.LinesInfo.Starting)
	if startingLen == 0 {
		return prevData, curData
	}

	prevLen := len(prevData)
	if prevLen > 0 {
		// starting symbol in the prevData
		idx := strings.Index(string(prevData), l.LinesInfo.Starting)
		if idx != -1 {
			return prevData[idx:], curData
		}

		// starting symbol in the middle of prevData and curData
		restStart := curData
		if len(curData) >= startingLen {
			restStart = curData[:startingLen-1]
		}
		prevData = append(prevData, restStart...)
		idx = strings.Index(string(prevData), l.LinesInfo.Starting)
		if idx != -1 {
			return prevData[idx:prevLen], curData
		}
	}

	// starting symbol in the curData
	idx := strings.Index(string(curData), l.LinesInfo.Starting)
	if idx != -1 {
		return nil, curData[idx:]
	}

	// no starting symbol
	if len(curData) >= startingLen {
		curData = curData[len(curData)-startingLen+1:]
	}
	return nil, curData
}

// getLine returns a line, curData, the next data start index and a bool value.
// If it has starting symbol the bool is true, otherwise is false.
func (l *LoadDataInfo) getLine(prevData, curData []byte) ([]byte, []byte, bool) {
	startingLen := len(l.LinesInfo.Starting)
	prevData, curData = l.getValidData(prevData, curData)
	if prevData == nil && len(curData) < startingLen {
		return nil, curData, false
	}
	prevLen := len(prevData)
	terminatedLen := len(l.LinesInfo.Terminated)
	curStartIdx := 0
	if prevLen < startingLen {
		curStartIdx = startingLen - prevLen
	}
	endIdx := -1
	if len(curData) >= curStartIdx {
		endIdx = strings.Index(string(curData[curStartIdx:]), l.LinesInfo.Terminated)
	}
	if endIdx == -1 {
		// no terminated symbol
		if len(prevData) == 0 {
			return nil, curData, true
		}

		// terminated symbol in the middle of prevData and curData
		curData = append(prevData, curData...)
		endIdx = strings.Index(string(curData[startingLen:]), l.LinesInfo.Terminated)
		if endIdx != -1 {
			nextDataIdx := startingLen + endIdx + terminatedLen
			return curData[startingLen : startingLen+endIdx], curData[nextDataIdx:], true
		}
		// no terminated symbol
		return nil, curData, true
	}

	// terminated symbol in the curData
	nextDataIdx := curStartIdx + endIdx + terminatedLen
	if len(prevData) == 0 {
		return curData[curStartIdx : curStartIdx+endIdx], curData[nextDataIdx:], true
	}

	// terminated symbol in the curData
	prevData = append(prevData, curData[:nextDataIdx]...)
	endIdx = strings.Index(string(prevData[startingLen:]), l.LinesInfo.Terminated)
	if endIdx >= prevLen {
		return prevData[startingLen : startingLen+endIdx], curData[nextDataIdx:], true
	}

	// terminated symbol in the middle of prevData and curData
	lineLen := startingLen + endIdx + terminatedLen
	return prevData[startingLen : startingLen+endIdx], curData[lineLen-prevLen:], true
}

func (l *LoadDataInfo) MysqlEscap(source string) (string, error) {
	var j = 0
	if len(source) == 0 {
		return "", errors.New("source is null")
	}
	tempStr := source[:]
	desc := make([]byte, len(tempStr)*2)
	for i := 0; i < len(tempStr); i++ {
		flag := false
		var escape byte
		switch tempStr[i] {
		case '\r':
			flag = true
			escape = '\r'
			break
		case '\n':
			flag = true
			escape = '\n'
			break
		case '\\':
			flag = true
			escape = '\\'
			break
		case '\'':
			flag = true
			escape = '\''
			break
		case '"':
			flag = true
			escape = '"'
			break
		case '\032':
			flag = true
			escape = 'Z'
			break
		default:
		}
		if flag {
			desc[j] = '\\'
			desc[j+1] = escape
			j = j + 2
		} else {
			desc[j] = tempStr[i]
			j = j + 1
		}
	}
	return string(desc[0:j]), nil
}

func (l *LoadDataInfo) MakeInsert(rows [][]string, tb *vindexes.Table, fields []*querypb.Field) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}
	var insertVasSQLBuf bytes.Buffer
	insertVasSQLBuf.WriteString("INSERT  IGNORE  INTO ")
	insertVasSQLBuf.WriteString(l.Table.Name.String())
	var needResolveVindexType = false
	if tb.Keyspace.Sharded  {
		needResolveVindexType = true
	}

	//get fields type and make the  insert ignore into values(...)
	var vindexIdx = -1
	var vindexFiledType querypb.Type
	columns := l.Columns
	columnsSize := len(columns)
	if columns != nil && columnsSize > 0 {
		insertVasSQLBuf.WriteString("(")
		for key, value := range columns {
			if needResolveVindexType && strings.EqualFold(value.String(), tb.ColumnVindexes[0].Columns[0].String()) {
				vindexIdx = key
				for _, field := range fields {
					if strings.EqualFold(field.Name, value.String()) {
						vindexFiledType = field.Type
						break
					}
				}
			}
			insertVasSQLBuf.WriteString(value.String())
			if key != columnsSize-1 {
				insertVasSQLBuf.WriteString(",")
			}
		}
		insertVasSQLBuf.WriteString(") ")
	}
	insertVasSQLBuf.WriteString(" values ")
	for k, record := range rows {
		insertVasSQLBuf.WriteString("(")
		for j := 0; j < columnsSize; j++ {

			var column string
			if j >= len(record) {
				column = ""
			} else {
				column = record[j]
			}

			// Distinguish the type of split field
			if needResolveVindexType && j == vindexIdx && isNumericType(vindexFiledType) {
				insertVasSQLBuf.WriteString(" ")
			} else {
				insertVasSQLBuf.WriteString("'")
			}
			insertVasSQLBuf.WriteString(column)

			if needResolveVindexType && j == vindexIdx && isNumericType(vindexFiledType) {
				insertVasSQLBuf.WriteString(" ")
			} else {
				insertVasSQLBuf.WriteString("'")
			}

			if j != columnsSize-1 {
				insertVasSQLBuf.WriteString(",")
			}
		}

		if k == len(rows)-1 {
			insertVasSQLBuf.WriteString(")")
		} else {
			insertVasSQLBuf.WriteString("),")
		}
	}
	return insertVasSQLBuf.String(), nil
}

// GetRowFromLine splits line according to fieldsInfo, this function is exported for testing.
func (l *LoadDataInfo) GetRowFromLine(line []byte) ([]string, error) {
	var sep []byte
	if l.FieldsInfo.Enclosed != 0 {
		if line[0] != l.FieldsInfo.Enclosed || line[len(line)-1] != l.FieldsInfo.Enclosed {
			return nil, vterrors.Errorf(vtrpcpb.Code_UNKNOWN, "line %s should begin and end with %V", string(line), l.FieldsInfo.Enclosed)
		}
		line = line[1 : len(line)-1]
		sep = make([]byte, 0, len(l.FieldsInfo.Terminated)+2)
		sep = append(sep, l.FieldsInfo.Enclosed)
		sep = append(sep, l.FieldsInfo.Terminated...)
		sep = append(sep, l.FieldsInfo.Enclosed)
	} else {
		sep = []byte(l.FieldsInfo.Terminated)
	}
	rawCols := bytes.Split(line, sep)
	cols := escapeCols(rawCols)
	return cols, nil
}

// InsertData inserts data into specified table according to the specified format.
// If prevData isn't nil and curData is nil, there are no other data to deal with and the isEOF is true
func (l *LoadDataInfo) InsertData(prevData, curData []byte, rows *[][]string, tb *vindexes.Table, fields []*querypb.Field, callback InsertFunc) ([]byte, bool, error) {
	// TODO: support enclosed and escape.
	if len(prevData) == 0 && len(curData) == 0 {
		return nil, false, nil
	}
	var line []byte
	var isEOF, hasStarting, reachLimit bool
	if len(prevData) > 0 && len(curData) == 0 {
		isEOF = true
		prevData, curData = curData, prevData
	}
	for len(curData) > 0 {
		line, curData, hasStarting = l.getLine(prevData, curData)
		prevData = nil
		// If it doesn't find the terminated symbol and this data isn't the last data,
		// the data can't be inserted.
		if line == nil && !isEOF {
			break
		}
		// If doesn't find starting symbol, this data can't be inserted.
		if !hasStarting {
			if isEOF {
				curData = nil
			}
			break
		}
		if line == nil && isEOF {
			line = curData[len(l.LinesInfo.Starting):]
			curData = nil
		}

		cols, err := l.GetRowFromLine(line)
		if err != nil {
			return nil, false, err
		}
		*rows = append(*rows, cols)
		if l.maxRowsInBatch != 0 && len(*rows) == l.maxRowsInBatch {
			reachLimit = true
			if inserts, err := l.MakeInsert(*rows, tb, fields); err != nil {
				return nil, false, err
			} else {
				*rows = make([][]string, 0)
				if err := callback(inserts); err != nil {
					return nil, false, err
				}
				break
			}
		}
	}
	return curData, reachLimit, nil
}

func (l *LoadDataInfo) insertDataWithCommit(prevData, curData []byte,  rows *[][]string, tb *vindexes.Table, fields []*querypb.Field, callback InsertFunc) ([]byte, error) {
	var err error
	var reachLimit bool
	for {
		prevData, reachLimit, err = l.InsertData(prevData, curData, rows, tb, fields, callback)
		if err != nil {
			return nil, err
		}
		if !reachLimit {
			break
		}
		curData = prevData
		prevData = nil
	}
	return prevData, nil
}

// LoadDataInfileDataStream load the cvs content
//func (l *LoadDataInfo) LoadDataInfileDataStream( /*data []byte,*/ctx context.Context, c *mysqlconn.Conn, bindVariables map[string]interface{}, session *vtgate.Session, tabletType topodata.TabletType) (*sqltypes.Result, error) {
//	loadRes := &sqltypes.Result{}
//	// Add some kind of timeout too.
//	var shouldBreak bool
//	var prevData, curData []byte
//	tableName := l.Table.Name.String()
//	tb, err := vh.vtg.router.planner.vschema.Find(c.SchemaName, tableName)
//	if err != nil {
//		return nil, fmt.Errorf("table %s not found", tableName)
//	}
//	var fields []*querypb.Field
//	if tb.Keyspace.Sharded && !tb.SingleShard {
//		qr, err := vh.vtg.router.GetFields(ctx, fmt.Sprintf("SELECT * FROM %s", tableName),
//			bindVariables, c.SchemaName, tabletType, session, true, &querypb.ExecuteOptions{IncludedFields: querypb.ExecuteOptions_ALL})
//		if err != nil {
//			return nil, err
//		}
//		fields = qr.Fields
//	}
//	var rows = make([][]string, 0)
//	for {
//		curData, err = c.ReadPacket()
//		if err != nil {
//			if err == io.EOF {
//				log.Error(err)
//				c.LoadDataDone = true
//				break
//			}
//		}
//
//		if len(curData) == 0 {
//			shouldBreak = true
//			if len(prevData) == 0 {
//				c.LoadDataDone = true
//				break
//			}
//		}
//		if prevData, err = l.insertDataWithCommit(prevData, curData, &rows, tb, fields, func(insert string) error {
//			// load data retry ExecuteMerge
//			var result *sqltypes.Result
//			if insert == "" {
//				return nil
//			}
//			result, err = vh.LoadDataRetry(ctx, c, insert, bindVariables, tabletType, session)
//			if err != nil {
//				return err
//			}
//			loadRes.InsertID = result.InsertID
//			loadRes.RowsAffected += result.RowsAffected
//			loadRes.Fields = result.Fields
//			return nil
//		}); err != nil {
//			return nil, err
//		}
//		if shouldBreak {
//			c.LoadDataDone = true
//			break
//		}
//	}
//	if len(rows) > 0 {
//		if inserts, err := l.MakeInsert(rows, tb, fields); err != nil {
//			return nil, err
//		} else {
//			result, err := vh.LoadDataRetry(ctx, c, inserts, bindVariables, tabletType, session)
//			if err != nil {
//				return nil, err
//			}
//			loadRes.InsertID = result.InsertID
//			loadRes.RowsAffected += result.RowsAffected
//			loadRes.Fields = result.Fields
//		}
//	}
//	return loadRes, nil
//}

func escapeCols(strs [][]byte) []string {
	ret := make([]string, len(strs))
	for i, v := range strs {
		output := escape(v)
		ret[i] = string(output)
	}
	return ret
}

// escape handles escape characters when running load data statement.
// TODO: escape need to be improved, it should support ESCAPED BY to specify
// the escape character and handle \N escape.
// See http://dev.mysql.com/doc/refman/5.7/en/load-data.html
func escape(str []byte) []byte {
	desc := make([]byte, len(str)*2)
	pos := 0
	for i := 0; i < len(str); i++ {
		c := str[i]
		if c == '\\' && i+1 < len(str) {
			c = sqlparser.EscapeChar(str[i+1])
			desc[pos] = c
			i++
			pos++
		} else if c == '"' || c == '\'' {
			desc[pos] = '\\'
			desc[pos+1] = c
			pos += 2
		} else {
			desc[pos] = c
			pos++
		}
	}
	return desc[:pos]
}

func isNumericType(t querypb.Type) bool {
	if t == querypb.Type_INT8 ||
		t == querypb.Type_INT16 ||
		t == querypb.Type_INT24 ||
		t == querypb.Type_INT32 ||
		t == querypb.Type_INT64 ||
		t == querypb.Type_UINT8 ||
		t == querypb.Type_UINT16 ||
		t == querypb.Type_UINT24 ||
		t == querypb.Type_UINT32 ||
		t == querypb.Type_UINT64 ||
		t == querypb.Type_FLOAT32 ||
		t == querypb.Type_FLOAT64 ||
		t == querypb.Type_DECIMAL {
		return true
	}
	return false
}
