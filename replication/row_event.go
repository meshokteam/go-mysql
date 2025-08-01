package replication

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/bits"
	"strconv"
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/shopspring/decimal"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/utils"
)

var errMissingTableMapEvent = errors.New("invalid table id, no corresponding table map event")

type TableMapEvent struct {
	flavor      string
	tableIDSize int

	TableID uint64

	Flags uint16

	Schema []byte
	Table  []byte

	ColumnCount uint64
	ColumnType  []byte
	ColumnMeta  []uint16

	// len = (ColumnCount + 7) / 8
	NullBitmap []byte

	/*
		The following are available only after MySQL-8.0.1 or MariaDB-10.5.0
		By default MySQL and MariaDB do not log the full row metadata.
		see:
			- https://dev.mysql.com/doc/refman/8.0/en/replication-options-binary-log.html#sysvar_binlog_row_metadata
			- https://mariadb.com/kb/en/replication-and-binary-log-system-variables/#binlog_row_metadata
	*/

	// SignednessBitmap stores signedness info for numeric columns.
	SignednessBitmap []byte

	// DefaultCharset/ColumnCharset stores collation info for character columns.

	// DefaultCharset[0] is the default collation of character columns.
	// For character columns that have different charset,
	// (character column index, column collation) pairs follows
	DefaultCharset []uint64
	// ColumnCharset contains collation sequence for all character columns
	ColumnCharset []uint64

	// SetStrValue stores values for set columns.
	SetStrValue       [][][]byte
	setStrValueString [][]string

	// EnumStrValue stores values for enum columns.
	EnumStrValue       [][][]byte
	enumStrValueString [][]string

	// ColumnName list all column names.
	ColumnName       [][]byte
	columnNameString []string // the same as ColumnName in string type, just for reuse

	// GeometryType stores real type for geometry columns.
	GeometryType []uint64

	// PrimaryKey is a sequence of column indexes of primary key.
	PrimaryKey []uint64

	// PrimaryKeyPrefix is the prefix length used for each column of primary key.
	// 0 means that the whole column length is used.
	PrimaryKeyPrefix []uint64

	// EnumSetDefaultCharset/EnumSetColumnCharset is similar to DefaultCharset/ColumnCharset but for enum/set columns.
	EnumSetDefaultCharset []uint64
	EnumSetColumnCharset  []uint64

	// VisibilityBitmap stores bits that are set if corresponding column is not invisible (MySQL 8.0.23+)
	VisibilityBitmap []byte

	optionalMetaDecodeFunc func(data []byte) (err error)
}

func (e *TableMapEvent) Decode(data []byte) error {
	pos := 0
	e.TableID = mysql.FixedLengthInt(data[0:e.tableIDSize])
	pos += e.tableIDSize

	e.Flags = binary.LittleEndian.Uint16(data[pos:])
	pos += 2

	schemaLength := data[pos]
	pos++

	e.Schema = data[pos : pos+int(schemaLength)]
	pos += int(schemaLength)

	// skip 0x00
	pos++

	tableLength := data[pos]
	pos++

	e.Table = data[pos : pos+int(tableLength)]
	pos += int(tableLength)

	// skip 0x00
	pos++

	var n int
	e.ColumnCount, _, n = mysql.LengthEncodedInt(data[pos:])
	pos += n

	e.ColumnType = data[pos : pos+int(e.ColumnCount)]
	pos += int(e.ColumnCount)

	var err error
	var metaData []byte
	if metaData, _, n, err = mysql.LengthEncodedString(data[pos:]); err != nil {
		return errors.Trace(err)
	}

	if err = e.decodeMeta(metaData); err != nil {
		return errors.Trace(err)
	}

	pos += n

	nullBitmapSize := bitmapByteSize(int(e.ColumnCount))
	if len(data[pos:]) < nullBitmapSize {
		return io.EOF
	}

	e.NullBitmap = data[pos : pos+nullBitmapSize]

	pos += nullBitmapSize

	if e.optionalMetaDecodeFunc != nil {
		if err = e.optionalMetaDecodeFunc(data[pos:]); err != nil {
			return err
		}
	} else {
		if err = e.decodeOptionalMeta(data[pos:]); err != nil {
			return err
		}
	}

	return nil
}

func bitmapByteSize(columnCount int) int {
	return (columnCount + 7) / 8
}

// see mysql sql/log_event.h
/*
	0 byte
	MYSQL_TYPE_DECIMAL
	MYSQL_TYPE_TINY
	MYSQL_TYPE_SHORT
	MYSQL_TYPE_LONG
	MYSQL_TYPE_NULL
	MYSQL_TYPE_TIMESTAMP
	MYSQL_TYPE_LONGLONG
	MYSQL_TYPE_INT24
	MYSQL_TYPE_DATE
	MYSQL_TYPE_TIME
	MYSQL_TYPE_DATETIME
	MYSQL_TYPE_YEAR

	1 byte
	MYSQL_TYPE_FLOAT
	MYSQL_TYPE_DOUBLE
	MYSQL_TYPE_BLOB
	MYSQL_TYPE_GEOMETRY
	MYSQL_TYPE_VECTOR

	//maybe
	MYSQL_TYPE_TIME2
	MYSQL_TYPE_DATETIME2
	MYSQL_TYPE_TIMESTAMP2

	2 byte
	MYSQL_TYPE_VARCHAR
	MYSQL_TYPE_BIT
	MYSQL_TYPE_NEWDECIMAL
	MYSQL_TYPE_VAR_STRING
	MYSQL_TYPE_STRING

	This enumeration value is only used internally and cannot exist in a binlog.
	MYSQL_TYPE_NEWDATE
	MYSQL_TYPE_ENUM
	MYSQL_TYPE_SET
	MYSQL_TYPE_TINY_BLOB
	MYSQL_TYPE_MEDIUM_BLOB
	MYSQL_TYPE_LONG_BLOB
*/
func (e *TableMapEvent) decodeMeta(data []byte) error {
	pos := 0
	e.ColumnMeta = make([]uint16, e.ColumnCount)
	for i, t := range e.ColumnType {
		switch t {
		case mysql.MYSQL_TYPE_STRING:
			x := uint16(data[pos]) << 8 // real type
			x += uint16(data[pos+1])    // pack or field length
			e.ColumnMeta[i] = x
			pos += 2
		case mysql.MYSQL_TYPE_NEWDECIMAL:
			x := uint16(data[pos]) << 8 // precision
			x += uint16(data[pos+1])    // decimals
			e.ColumnMeta[i] = x
			pos += 2
		case mysql.MYSQL_TYPE_VAR_STRING,
			mysql.MYSQL_TYPE_VARCHAR,
			mysql.MYSQL_TYPE_BIT:
			e.ColumnMeta[i] = binary.LittleEndian.Uint16(data[pos:])
			pos += 2
		case mysql.MYSQL_TYPE_BLOB,
			mysql.MYSQL_TYPE_DOUBLE,
			mysql.MYSQL_TYPE_FLOAT,
			mysql.MYSQL_TYPE_GEOMETRY,
			mysql.MYSQL_TYPE_VECTOR,
			mysql.MYSQL_TYPE_JSON:
			e.ColumnMeta[i] = uint16(data[pos])
			pos++
		case mysql.MYSQL_TYPE_TIME2,
			mysql.MYSQL_TYPE_DATETIME2,
			mysql.MYSQL_TYPE_TIMESTAMP2:
			e.ColumnMeta[i] = uint16(data[pos])
			pos++
		case mysql.MYSQL_TYPE_NEWDATE,
			mysql.MYSQL_TYPE_ENUM,
			mysql.MYSQL_TYPE_SET,
			mysql.MYSQL_TYPE_TINY_BLOB,
			mysql.MYSQL_TYPE_MEDIUM_BLOB,
			mysql.MYSQL_TYPE_LONG_BLOB:
			return errors.Errorf("unsupport type in binlog %d", t)
		default:
			e.ColumnMeta[i] = 0
		}
	}

	return nil
}

func (e *TableMapEvent) decodeOptionalMeta(data []byte) (err error) {
	pos := 0
	for pos < len(data) {
		// optional metadata fields are stored in Type, Length, Value(TLV) format
		// Type takes 1 byte. Length is a packed integer value. Values takes Length bytes
		t := data[pos]
		pos++

		l, _, n := mysql.LengthEncodedInt(data[pos:])
		pos += n

		v := data[pos : pos+int(l)]
		pos += int(l)

		switch t {
		case TABLE_MAP_OPT_META_SIGNEDNESS:
			e.SignednessBitmap = v

		case TABLE_MAP_OPT_META_DEFAULT_CHARSET:
			e.DefaultCharset, err = e.decodeDefaultCharset(v)
			if err != nil {
				return err
			}

		case TABLE_MAP_OPT_META_COLUMN_CHARSET:
			e.ColumnCharset, err = e.decodeIntSeq(v)
			if err != nil {
				return err
			}

		case TABLE_MAP_OPT_META_COLUMN_NAME:
			if err = e.decodeColumnNames(v); err != nil {
				return err
			}

		case TABLE_MAP_OPT_META_SET_STR_VALUE:
			e.SetStrValue, err = e.decodeStrValue(v)
			if err != nil {
				return err
			}

		case TABLE_MAP_OPT_META_ENUM_STR_VALUE:
			e.EnumStrValue, err = e.decodeStrValue(v)
			if err != nil {
				return err
			}

		case TABLE_MAP_OPT_META_GEOMETRY_TYPE:
			e.GeometryType, err = e.decodeIntSeq(v)
			if err != nil {
				return err
			}

		case TABLE_MAP_OPT_META_SIMPLE_PRIMARY_KEY:
			if err = e.decodeSimplePrimaryKey(v); err != nil {
				return err
			}

		case TABLE_MAP_OPT_META_PRIMARY_KEY_WITH_PREFIX:
			if err = e.decodePrimaryKeyWithPrefix(v); err != nil {
				return err
			}

		case TABLE_MAP_OPT_META_ENUM_AND_SET_DEFAULT_CHARSET:
			e.EnumSetDefaultCharset, err = e.decodeDefaultCharset(v)
			if err != nil {
				return err
			}

		case TABLE_MAP_OPT_META_ENUM_AND_SET_COLUMN_CHARSET:
			e.EnumSetColumnCharset, err = e.decodeIntSeq(v)
			if err != nil {
				return err
			}

		case TABLE_MAP_OPT_META_COLUMN_VISIBILITY:
			e.VisibilityBitmap = v

		default:
			// Ignore for future extension
		}
	}

	return nil
}

func (e *TableMapEvent) decodeIntSeq(v []byte) (ret []uint64, err error) {
	p := 0
	for p < len(v) {
		i, _, n := mysql.LengthEncodedInt(v[p:])
		p += n
		ret = append(ret, i)
	}
	return
}

func (e *TableMapEvent) decodeDefaultCharset(v []byte) (ret []uint64, err error) {
	ret, err = e.decodeIntSeq(v)
	if err != nil {
		return
	}
	if len(ret)%2 != 1 {
		return nil, errors.Errorf("Expect odd item in DefaultCharset but got %d", len(ret))
	}
	return
}

func (e *TableMapEvent) decodeColumnNames(v []byte) error {
	p := 0
	e.ColumnName = make([][]byte, 0, e.ColumnCount)
	for p < len(v) {
		n := int(v[p])
		p++
		e.ColumnName = append(e.ColumnName, v[p:p+n])
		p += n
	}

	if len(e.ColumnName) != int(e.ColumnCount) {
		return errors.Errorf("Expect %d column names but got %d", e.ColumnCount, len(e.ColumnName))
	}
	return nil
}

func (e *TableMapEvent) decodeStrValue(v []byte) (ret [][][]byte, err error) {
	p := 0
	for p < len(v) {
		nVal, _, n := mysql.LengthEncodedInt(v[p:])
		p += n
		vals := make([][]byte, 0, int(nVal))
		for i := 0; i < int(nVal); i++ {
			val, _, n, err := mysql.LengthEncodedString(v[p:])
			if err != nil {
				return nil, err
			}
			p += n
			vals = append(vals, val)
		}
		ret = append(ret, vals)
	}
	return
}

func (e *TableMapEvent) decodeSimplePrimaryKey(v []byte) error {
	p := 0
	for p < len(v) {
		i, _, n := mysql.LengthEncodedInt(v[p:])
		e.PrimaryKey = append(e.PrimaryKey, i)
		e.PrimaryKeyPrefix = append(e.PrimaryKeyPrefix, 0)
		p += n
	}
	return nil
}

func (e *TableMapEvent) decodePrimaryKeyWithPrefix(v []byte) error {
	p := 0
	for p < len(v) {
		i, _, n := mysql.LengthEncodedInt(v[p:])
		e.PrimaryKey = append(e.PrimaryKey, i)
		p += n
		i, _, n = mysql.LengthEncodedInt(v[p:])
		e.PrimaryKeyPrefix = append(e.PrimaryKeyPrefix, i)
		p += n
	}
	return nil
}

func (e *TableMapEvent) Dump(w io.Writer) {
	fmt.Fprintf(w, "TableID: %d\n", e.TableID)
	fmt.Fprintf(w, "TableID size: %d\n", e.tableIDSize)
	fmt.Fprintf(w, "Flags: %d\n", e.Flags)
	fmt.Fprintf(w, "Schema: %s\n", e.Schema)
	fmt.Fprintf(w, "Table: %s\n", e.Table)
	fmt.Fprintf(w, "Column count: %d\n", e.ColumnCount)
	fmt.Fprintf(w, "Column type: \n%s", hex.Dump(e.ColumnType))
	fmt.Fprintf(w, "NULL bitmap: \n%s", hex.Dump(e.NullBitmap))

	fmt.Fprintf(w, "Signedness bitmap: \n%s", hex.Dump(e.SignednessBitmap))
	fmt.Fprintf(w, "Default charset: %v\n", e.DefaultCharset)
	fmt.Fprintf(w, "Column charset: %v\n", e.ColumnCharset)
	fmt.Fprintf(w, "Set str value: %v\n", e.SetStrValueString())
	fmt.Fprintf(w, "Enum str value: %v\n", e.EnumStrValueString())
	fmt.Fprintf(w, "Column name: %v\n", e.ColumnNameString())
	fmt.Fprintf(w, "Geometry type: %v\n", e.GeometryType)
	fmt.Fprintf(w, "Primary key: %v\n", e.PrimaryKey)
	fmt.Fprintf(w, "Primary key prefix: %v\n", e.PrimaryKeyPrefix)
	fmt.Fprintf(w, "Enum/set default charset: %v\n", e.EnumSetDefaultCharset)
	fmt.Fprintf(w, "Enum/set column charset: %v\n", e.EnumSetColumnCharset)
	fmt.Fprintf(w, "Invisible Column bitmap: \n%s", hex.Dump(e.VisibilityBitmap))

	unsignedMap := e.UnsignedMap()
	fmt.Fprintf(w, "UnsignedMap: %#v\n", unsignedMap)

	collationMap := e.CollationMap()
	fmt.Fprintf(w, "CollationMap: %#v\n", collationMap)

	enumSetCollationMap := e.EnumSetCollationMap()
	fmt.Fprintf(w, "EnumSetCollationMap: %#v\n", enumSetCollationMap)

	enumStrValueMap := e.EnumStrValueMap()
	fmt.Fprintf(w, "EnumStrValueMap: %#v\n", enumStrValueMap)

	setStrValueMap := e.SetStrValueMap()
	fmt.Fprintf(w, "SetStrValueMap: %#v\n", setStrValueMap)

	geometryTypeMap := e.GeometryTypeMap()
	fmt.Fprintf(w, "GeometryTypeMap: %#v\n", geometryTypeMap)

	visibilityMap := e.VisibilityMap()
	fmt.Fprintf(w, "VisibilityMap: %#v\n", visibilityMap)

	nameMaxLen := 0
	for _, name := range e.ColumnName {
		if len(name) > nameMaxLen {
			nameMaxLen = len(name)
		}
	}
	nameFmt := "  %s"
	if nameMaxLen > 0 {
		nameFmt = fmt.Sprintf("  %%-%ds", nameMaxLen)
	}

	primaryKey := map[int]struct{}{}
	for _, pk := range e.PrimaryKey {
		primaryKey[int(pk)] = struct{}{}
	}

	fmt.Fprintf(w, "Columns: \n")
	for i := 0; i < int(e.ColumnCount); i++ {
		if len(e.ColumnName) == 0 {
			fmt.Fprintf(w, nameFmt, "<n/a>")
		} else {
			fmt.Fprintf(w, nameFmt, e.ColumnName[i])
		}

		fmt.Fprintf(w, "  type=%-3d", e.realType(i))

		if e.IsNumericColumn(i) {
			if len(unsignedMap) == 0 {
				fmt.Fprintf(w, "  unsigned=<n/a>")
			} else if unsignedMap[i] {
				fmt.Fprintf(w, "  unsigned=yes")
			} else {
				fmt.Fprintf(w, "  unsigned=no ")
			}
		}
		if e.IsCharacterColumn(i) {
			if len(collationMap) == 0 {
				fmt.Fprintf(w, "  collation=<n/a>")
			} else {
				fmt.Fprintf(w, "  collation=%d ", collationMap[i])
			}
		}
		if e.IsEnumColumn(i) {
			if len(enumSetCollationMap) == 0 {
				fmt.Fprintf(w, "  enum_collation=<n/a>")
			} else {
				fmt.Fprintf(w, "  enum_collation=%d", enumSetCollationMap[i])
			}

			if len(enumStrValueMap) == 0 {
				fmt.Fprintf(w, "  enum=<n/a>")
			} else {
				fmt.Fprintf(w, "  enum=%v", enumStrValueMap[i])
			}
		}
		if e.IsSetColumn(i) {
			if len(enumSetCollationMap) == 0 {
				fmt.Fprintf(w, "  set_collation=<n/a>")
			} else {
				fmt.Fprintf(w, "  set_collation=%d", enumSetCollationMap[i])
			}

			if len(setStrValueMap) == 0 {
				fmt.Fprintf(w, "  set=<n/a>")
			} else {
				fmt.Fprintf(w, "  set=%v", setStrValueMap[i])
			}
		}
		if e.IsGeometryColumn(i) {
			if len(geometryTypeMap) == 0 {
				fmt.Fprintf(w, "  geometry_type=<n/a>")
			} else {
				fmt.Fprintf(w, "  geometry_type=%v", geometryTypeMap[i])
			}
		}

		available, nullable := e.Nullable(i)
		if !available {
			fmt.Fprintf(w, "  null=<n/a>")
		} else if nullable {
			fmt.Fprintf(w, "  null=yes")
		} else {
			fmt.Fprintf(w, "  null=no ")
		}

		if _, ok := primaryKey[i]; ok {
			fmt.Fprintf(w, "  pri")
		}

		fmt.Fprintf(w, "\n")
	}

	fmt.Fprintln(w)
}

// Nullable returns the nullablity of the i-th column.
// If null bits are not available, available is false.
// i must be in range [0, ColumnCount).
func (e *TableMapEvent) Nullable(i int) (available, nullable bool) {
	if len(e.NullBitmap) == 0 {
		return
	}
	return true, e.NullBitmap[i/8]&(1<<uint(i%8)) != 0
}

// SetStrValueString returns values for set columns as string slices.
// nil is returned if not available or no set columns at all.
func (e *TableMapEvent) SetStrValueString() [][]string {
	if e.setStrValueString == nil {
		if len(e.SetStrValue) == 0 {
			return nil
		}
		e.setStrValueString = make([][]string, len(e.SetStrValue))
		for i, vals := range e.SetStrValue {
			e.setStrValueString[i] = e.bytesSlice2StrSlice(vals)
		}
	}
	return e.setStrValueString
}

// EnumStrValueString returns values for enum columns as string slices.
// nil is returned if not available or no enum columns at all.
func (e *TableMapEvent) EnumStrValueString() [][]string {
	if e.enumStrValueString == nil {
		if len(e.EnumStrValue) == 0 {
			return nil
		}
		e.enumStrValueString = make([][]string, len(e.EnumStrValue))
		for i, vals := range e.EnumStrValue {
			e.enumStrValueString[i] = e.bytesSlice2StrSlice(vals)
		}
	}
	return e.enumStrValueString
}

// ColumnNameString returns column names as string slice.
// nil is returned if not available.
func (e *TableMapEvent) ColumnNameString() []string {
	if e.columnNameString == nil {
		e.columnNameString = e.bytesSlice2StrSlice(e.ColumnName)
	}
	return e.columnNameString
}

func (e *TableMapEvent) bytesSlice2StrSlice(src [][]byte) []string {
	if src == nil {
		return nil
	}
	ret := make([]string, len(src))
	for i, item := range src {
		ret[i] = string(item)
	}
	return ret
}

// UnsignedMap returns a map: column index -> unsigned.
// Note that only numeric columns will be returned.
// nil is returned if not available or no numeric columns at all.
func (e *TableMapEvent) UnsignedMap() map[int]bool {
	if len(e.SignednessBitmap) == 0 {
		return nil
	}
	ret := make(map[int]bool)
	i := 0
	for _, field := range e.SignednessBitmap {
		for c := 0x80; c != 0; {
			if e.IsNumericColumn(i) {
				ret[i] = field&byte(c) != 0
				c >>= 1
			}
			i++
			if i >= int(e.ColumnCount) {
				return ret
			}
		}
	}
	return ret
}

// CollationMap returns a map: column index -> collation id.
// Note that only character columns will be returned.
// nil is returned if not available or no character columns at all.
func (e *TableMapEvent) CollationMap() map[int]uint64 {
	return e.collationMap(e.IsCharacterColumn, e.DefaultCharset, e.ColumnCharset)
}

// EnumSetCollationMap returns a map: column index -> collation id.
// Note that only enum or set columns will be returned.
// nil is returned if not available or no enum/set columns at all.
func (e *TableMapEvent) EnumSetCollationMap() map[int]uint64 {
	return e.collationMap(e.IsEnumOrSetColumn, e.EnumSetDefaultCharset, e.EnumSetColumnCharset)
}

func (e *TableMapEvent) collationMap(includeType func(int) bool, defaultCharset, columnCharset []uint64) map[int]uint64 {
	if len(defaultCharset) != 0 {
		defaultCollation := defaultCharset[0]

		// character column index -> collation
		collations := make(map[int]uint64)
		for i := 1; i < len(defaultCharset); i += 2 {
			collations[int(defaultCharset[i])] = defaultCharset[i+1]
		}

		p := 0
		ret := make(map[int]uint64)
		for i := 0; i < int(e.ColumnCount); i++ {
			if !includeType(i) {
				continue
			}

			if collation, ok := collations[p]; ok {
				ret[i] = collation
			} else {
				ret[i] = defaultCollation
			}
			p++
		}

		return ret
	}

	if len(columnCharset) != 0 {
		p := 0
		ret := make(map[int]uint64)
		for i := 0; i < int(e.ColumnCount); i++ {
			if !includeType(i) {
				continue
			}

			ret[i] = columnCharset[p]
			p++
		}

		return ret
	}

	return nil
}

// EnumStrValueMap returns a map: column index -> enum string value.
// Note that only enum columns will be returned.
// nil is returned if not available or no enum columns at all.
func (e *TableMapEvent) EnumStrValueMap() map[int][]string {
	return e.strValueMap(e.IsEnumColumn, e.EnumStrValueString())
}

// SetStrValueMap returns a map: column index -> set string value.
// Note that only set columns will be returned.
// nil is returned if not available or no set columns at all.
func (e *TableMapEvent) SetStrValueMap() map[int][]string {
	return e.strValueMap(e.IsSetColumn, e.SetStrValueString())
}

func (e *TableMapEvent) strValueMap(includeType func(int) bool, strValue [][]string) map[int][]string {
	if len(strValue) == 0 {
		return nil
	}
	p := 0
	ret := make(map[int][]string)
	for i := 0; i < int(e.ColumnCount); i++ {
		if !includeType(i) {
			continue
		}
		ret[i] = strValue[p]
		p++
	}
	return ret
}

// GeometryTypeMap returns a map: column index -> geometry type.
// Note that only geometry columns will be returned.
// nil is returned if not available or no geometry columns at all.
func (e *TableMapEvent) GeometryTypeMap() map[int]uint64 {
	if len(e.GeometryType) == 0 {
		return nil
	}
	p := 0
	ret := make(map[int]uint64)
	for i := 0; i < int(e.ColumnCount); i++ {
		if !e.IsGeometryColumn(i) {
			continue
		}

		ret[i] = e.GeometryType[p]
		p++
	}
	return ret
}

// VisibilityMap returns a map: column index -> visiblity.
// Invisible column was introduced in MySQL 8.0.23
// nil is returned if not available.
func (e *TableMapEvent) VisibilityMap() map[int]bool {
	if len(e.VisibilityBitmap) == 0 {
		return nil
	}
	ret := make(map[int]bool, len(e.VisibilityBitmap)*8)
	i := 0
	for _, field := range e.VisibilityBitmap {
		for c := 0x80; c != 0; c >>= 1 {
			ret[i] = field&byte(c) != 0
			i++
			if uint64(i) >= e.ColumnCount {
				return ret
			}
		}
	}
	return ret
}

// Below realType and IsXXXColumn are base from:
//   table_def::type in sql/rpl_utility.h
//   Table_map_log_event::print_columns in mysql-8.0/sql/log_event.cc and mariadb-10.5/sql/log_event_client.cc

func (e *TableMapEvent) realType(i int) byte {
	typ := e.ColumnType[i]

	switch typ {
	case mysql.MYSQL_TYPE_STRING:
		rtyp := byte(e.ColumnMeta[i] >> 8)
		if rtyp == mysql.MYSQL_TYPE_ENUM || rtyp == mysql.MYSQL_TYPE_SET {
			return rtyp
		}

	case mysql.MYSQL_TYPE_DATE:
		return mysql.MYSQL_TYPE_NEWDATE
	}

	return typ
}

func (e *TableMapEvent) IsNumericColumn(i int) bool {
	switch e.realType(i) {
	case mysql.MYSQL_TYPE_TINY,
		mysql.MYSQL_TYPE_SHORT,
		mysql.MYSQL_TYPE_INT24,
		mysql.MYSQL_TYPE_LONG,
		mysql.MYSQL_TYPE_LONGLONG,
		mysql.MYSQL_TYPE_NEWDECIMAL,
		mysql.MYSQL_TYPE_FLOAT,
		mysql.MYSQL_TYPE_DOUBLE:
		return true

	default:
		return false
	}
}

// IsCharacterColumn returns true if the column type is considered as character type.
// Note that JSON/GEOMETRY types are treated as character type in mariadb.
// (JSON is an alias for LONGTEXT in mariadb: https://mariadb.com/kb/en/json-data-type/)
func (e *TableMapEvent) IsCharacterColumn(i int) bool {
	switch e.realType(i) {
	case mysql.MYSQL_TYPE_STRING,
		mysql.MYSQL_TYPE_VAR_STRING,
		mysql.MYSQL_TYPE_VARCHAR,
		mysql.MYSQL_TYPE_BLOB:
		return true

	case mysql.MYSQL_TYPE_GEOMETRY:
		if e.flavor == "mariadb" {
			return true
		}
		return false

	default:
		return false
	}
}

func (e *TableMapEvent) IsEnumColumn(i int) bool {
	return e.realType(i) == mysql.MYSQL_TYPE_ENUM
}

func (e *TableMapEvent) IsSetColumn(i int) bool {
	return e.realType(i) == mysql.MYSQL_TYPE_SET
}

func (e *TableMapEvent) IsGeometryColumn(i int) bool {
	return e.realType(i) == mysql.MYSQL_TYPE_GEOMETRY
}

func (e *TableMapEvent) IsEnumOrSetColumn(i int) bool {
	rtyp := e.realType(i)
	return rtyp == mysql.MYSQL_TYPE_ENUM || rtyp == mysql.MYSQL_TYPE_SET
}

// JsonColumnCount returns the number of JSON columns in this table
func (e *TableMapEvent) JsonColumnCount() uint64 {
	count := uint64(0)
	for _, t := range e.ColumnType {
		if t == mysql.MYSQL_TYPE_JSON {
			count++
		}
	}

	return count
}

// RowsEventStmtEndFlag is set in the end of the statement.
const RowsEventStmtEndFlag = 0x01

// RowsEvent represents a MySQL rows event like DELETE_ROWS_EVENT,
// UPDATE_ROWS_EVENT, etc.
// RowsEvent.Rows saves the rows data, and the MySQL type to golang type mapping
// is
// - mysql.MYSQL_TYPE_NULL: nil
// - mysql.MYSQL_TYPE_LONG: int32
// - mysql.MYSQL_TYPE_TINY: int8
// - mysql.MYSQL_TYPE_SHORT: int16
// - mysql.MYSQL_TYPE_INT24: int32
// - mysql.MYSQL_TYPE_LONGLONG: int64
// - mysql.MYSQL_TYPE_NEWDECIMAL: string / "github.com/shopspring/decimal".Decimal
// - mysql.MYSQL_TYPE_FLOAT: float32
// - mysql.MYSQL_TYPE_DOUBLE: float64
// - mysql.MYSQL_TYPE_BIT: int64
// - mysql.MYSQL_TYPE_TIMESTAMP: string / time.Time
// - mysql.MYSQL_TYPE_TIMESTAMP2: string / time.Time
// - mysql.MYSQL_TYPE_DATETIME: string / time.Time
// - mysql.MYSQL_TYPE_DATETIME2: string / time.Time
// - mysql.MYSQL_TYPE_TIME: string
// - mysql.MYSQL_TYPE_TIME2: string
// - mysql.MYSQL_TYPE_DATE: string
// - mysql.MYSQL_TYPE_YEAR: int
// - mysql.MYSQL_TYPE_ENUM: int64
// - mysql.MYSQL_TYPE_SET: int64
// - mysql.MYSQL_TYPE_BLOB: []byte
// - mysql.MYSQL_TYPE_VARCHAR: string
// - mysql.MYSQL_TYPE_VAR_STRING: string
// - mysql.MYSQL_TYPE_STRING: string
// - mysql.MYSQL_TYPE_JSON: []byte / *replication.JsonDiff
// - mysql.MYSQL_TYPE_GEOMETRY: []byte
// - mysql.MYSQL_TYPE_VECTOR: []byte
type RowsEvent struct {
	// 0, 1, 2
	Version int

	tableIDSize int
	tables      map[uint64]*TableMapEvent
	needBitmap2 bool

	// for mariadb *_COMPRESSED_EVENT_V1
	compressed bool

	// raw event type associated with a RowsEvent
	eventType EventType

	Table *TableMapEvent

	TableID uint64

	Flags uint16

	// if version == 2
	// Use when DataLen value is greater than 2
	NdbFormat byte
	NdbData   []byte

	PartitionId       uint16
	SourcePartitionId uint16

	// lenenc_int
	ColumnCount uint64

	/*
		By default MySQL and MariaDB log the full row image.
		see
			- https://dev.mysql.com/doc/refman/8.0/en/replication-options-binary-log.html#sysvar_binlog_row_image
			- https://mariadb.com/kb/en/replication-and-binary-log-system-variables/#binlog_row_image

		ColumnBitmap1, ColumnBitmap2 and SkippedColumns are not set on the full row image.
	*/

	// len = (ColumnCount + 7) / 8
	ColumnBitmap1 []byte

	// if UPDATE_ROWS_EVENTv1 or v2, or PARTIAL_UPDATE_ROWS_EVENT
	// len = (ColumnCount + 7) / 8
	ColumnBitmap2 []byte

	// rows: all return types from RowsEvent.decodeValue()
	Rows           [][]interface{}
	SkippedColumns [][]int

	parseTime                bool
	timestampStringLocation  *time.Location
	useDecimal               bool
	useFloatWithTrailingZero bool
	ignoreJSONDecodeErr      bool
}

// EnumRowsEventType is an abridged type describing the operation which triggered the given RowsEvent.
type EnumRowsEventType byte

const (
	EnumRowsEventTypeUnknown = EnumRowsEventType(iota)
	EnumRowsEventTypeInsert
	EnumRowsEventTypeUpdate
	EnumRowsEventTypeDelete
)

func (t EnumRowsEventType) String() string {
	switch t {
	case EnumRowsEventTypeInsert:
		return "insert"
	case EnumRowsEventTypeUpdate:
		return "update"
	case EnumRowsEventTypeDelete:
		return "delete"
	default:
		return fmt.Sprintf("unknown (%d)", t)
	}
}

// EnumRowImageType is allowed types for every row in mysql binlog.
// See https://github.com/mysql/mysql-server/blob/1bfe02bdad6604d54913c62614bde57a055c8332/sql/rpl_record.h#L39
// enum class enum_row_image_type { WRITE_AI, UPDATE_BI, UPDATE_AI, DELETE_BI };
type EnumRowImageType byte

const (
	EnumRowImageTypeWriteAI = EnumRowImageType(iota)
	EnumRowImageTypeUpdateBI
	EnumRowImageTypeUpdateAI
	EnumRowImageTypeDeleteBI
)

func (t EnumRowImageType) String() string {
	switch t {
	case EnumRowImageTypeWriteAI:
		return "WriteAI"
	case EnumRowImageTypeUpdateBI:
		return "UpdateBI"
	case EnumRowImageTypeUpdateAI:
		return "UpdateAI"
	case EnumRowImageTypeDeleteBI:
		return "DeleteBI"
	default:
		return fmt.Sprintf("(%d)", t)
	}
}

// Bits for binlog_row_value_options sysvar
type EnumBinlogRowValueOptions byte

const (
	// Store JSON updates in partial form
	EnumBinlogRowValueOptionsPartialJsonUpdates = EnumBinlogRowValueOptions(iota + 1)
)

func (e *RowsEvent) DecodeHeader(data []byte) (int, error) {
	pos := 0
	e.TableID = mysql.FixedLengthInt(data[0:e.tableIDSize])
	pos += e.tableIDSize

	e.Flags = binary.LittleEndian.Uint16(data[pos:])
	pos += 2

	if e.Version == 2 {
		dataLen := binary.LittleEndian.Uint16(data[pos:])
		pos += 2
		if dataLen > 2 {
			err := e.decodeExtraData(data[pos:])
			if err != nil {
				return 0, err
			}
		}
		pos += int(dataLen - 2)
	}

	var n int
	e.ColumnCount, _, n = mysql.LengthEncodedInt(data[pos:])
	pos += n

	bitCount := bitmapByteSize(int(e.ColumnCount))
	e.ColumnBitmap1 = data[pos : pos+bitCount]
	pos += bitCount

	if e.needBitmap2 {
		e.ColumnBitmap2 = data[pos : pos+bitCount]
		pos += bitCount
	}

	var ok bool
	e.Table, ok = e.tables[e.TableID]
	if !ok {
		if len(e.tables) > 0 {
			return 0, errors.Errorf("invalid table id %d, no corresponding table map event", e.TableID)
		} else {
			return 0, errors.Annotatef(errMissingTableMapEvent, "table id %d", e.TableID)
		}
	}
	return pos, nil
}

func (e *RowsEvent) decodeExtraData(data []byte) (err2 error) {
	pos := 0
	extraDataType := data[pos]
	pos += 1
	switch extraDataType {
	case ENUM_EXTRA_ROW_INFO_TYPECODE_NDB:
		ndbLength := int(data[pos])
		pos += 1
		e.NdbFormat = data[pos]
		pos += 1
		e.NdbData = data[pos : pos+ndbLength-2]
	case ENUM_EXTRA_ROW_INFO_TYPECODE_PARTITION:
		if e.eventType == UPDATE_ROWS_EVENTv1 || e.eventType == UPDATE_ROWS_EVENTv2 || e.eventType == PARTIAL_UPDATE_ROWS_EVENT {
			e.PartitionId = binary.LittleEndian.Uint16(data[pos:])
			pos += 2
			e.SourcePartitionId = binary.LittleEndian.Uint16(data[pos:])
		} else {
			e.PartitionId = binary.LittleEndian.Uint16(data[pos:])
		}
	}
	return nil
}

func (e *RowsEvent) DecodeData(pos int, data []byte) (err2 error) {
	if e.compressed {
		data, err2 = mysql.DecompressMariadbData(data[pos:])
		if err2 != nil {
			//nolint:nakedret
			return
		}
		pos = 0
	}

	// Rows_log_event::print_verbose()

	var (
		n   int
		err error
	)
	// ... repeat rows until event-end
	defer func() {
		if r := recover(); r != nil {
			err2 = errors.Errorf("parse rows event panic %v, data %q, parsed rows %#v, table map %#v", r, data, e, e.Table)
		}
	}()

	// Pre-allocate memory for rows: before image + (optional) after image
	rowsLen := 1
	if e.needBitmap2 {
		rowsLen++
	}
	e.SkippedColumns = make([][]int, 0, rowsLen)
	e.Rows = make([][]interface{}, 0, rowsLen)

	var rowImageType EnumRowImageType
	switch e.eventType {
	case WRITE_ROWS_EVENTv0, WRITE_ROWS_EVENTv1, WRITE_ROWS_EVENTv2, MARIADB_WRITE_ROWS_COMPRESSED_EVENT_V1:
		rowImageType = EnumRowImageTypeWriteAI
	case DELETE_ROWS_EVENTv0, DELETE_ROWS_EVENTv1, DELETE_ROWS_EVENTv2, MARIADB_DELETE_ROWS_COMPRESSED_EVENT_V1:
		rowImageType = EnumRowImageTypeDeleteBI
	default:
		rowImageType = EnumRowImageTypeUpdateBI
	}

	for pos < len(data) {
		// Parse the first image
		if n, err = e.decodeImage(data[pos:], e.ColumnBitmap1, rowImageType); err != nil {
			return errors.Trace(err)
		}
		pos += n

		// Parse the second image (for UPDATE only)
		if e.needBitmap2 {
			if n, err = e.decodeImage(data[pos:], e.ColumnBitmap2, EnumRowImageTypeUpdateAI); err != nil {
				return errors.Trace(err)
			}
			pos += n
		}
	}

	return nil
}

func (e *RowsEvent) Decode(data []byte) error {
	pos, err := e.DecodeHeader(data)
	if err != nil {
		return err
	}
	return e.DecodeData(pos, data)
}

func (e *RowsEvent) Type() EnumRowsEventType {
	switch e.eventType {
	case WRITE_ROWS_EVENTv0, WRITE_ROWS_EVENTv1, WRITE_ROWS_EVENTv2, MARIADB_WRITE_ROWS_COMPRESSED_EVENT_V1:
		return EnumRowsEventTypeInsert
	case UPDATE_ROWS_EVENTv0, UPDATE_ROWS_EVENTv1, UPDATE_ROWS_EVENTv2, MARIADB_UPDATE_ROWS_COMPRESSED_EVENT_V1:
		return EnumRowsEventTypeUpdate
	case DELETE_ROWS_EVENTv0, DELETE_ROWS_EVENTv1, DELETE_ROWS_EVENTv2, MARIADB_DELETE_ROWS_COMPRESSED_EVENT_V1:
		return EnumRowsEventTypeDelete
	default:
		return EnumRowsEventTypeUnknown
	}
}

func isBitSet(bitmap []byte, i int) bool {
	return bitmap[i>>3]&(1<<(uint(i)&7)) > 0
}

func isBitSetIncr(bitmap []byte, i *int) bool {
	v := isBitSet(bitmap, *i)
	*i++
	return v
}

func (e *RowsEvent) decodeImage(data []byte, bitmap []byte, rowImageType EnumRowImageType) (int, error) {
	// Rows_log_event::print_verbose_one_row()

	pos := 0

	var isPartialJsonUpdate bool

	var partialBitmap []byte
	if e.eventType == PARTIAL_UPDATE_ROWS_EVENT && rowImageType == EnumRowImageTypeUpdateAI {
		binlogRowValueOptions, _, n := mysql.LengthEncodedInt(data[pos:]) // binlog_row_value_options
		pos += n
		isPartialJsonUpdate = EnumBinlogRowValueOptions(binlogRowValueOptions)&EnumBinlogRowValueOptionsPartialJsonUpdates != 0
		if isPartialJsonUpdate {
			byteCount := bitmapByteSize(int(e.Table.JsonColumnCount()))
			partialBitmap = data[pos : pos+byteCount]
			pos += byteCount
		}
	}

	row := make([]interface{}, e.ColumnCount)

	// refer: https://github.com/alibaba/canal/blob/c3e38e50e269adafdd38a48c63a1740cde304c67/dbsync/src/main/java/com/taobao/tddl/dbsync/binlog/event/RowsLogBuffer.java#L63
	count := 0
	col := 0
	for ; col+8 <= int(e.ColumnCount); col += 8 {
		count += bits.OnesCount8(bitmap[col>>3])
	}
	if col < int(e.ColumnCount) {
		count += bits.OnesCount8(bitmap[col>>3] & byte((1<<(int(e.ColumnCount)-col))-1))
	}
	skips := make([]int, 0, int(e.ColumnCount)-count)
	count = bitmapByteSize(count)

	nullBitmap := data[pos : pos+count]
	pos += count

	partialBitmapIndex := 0
	nullBitmapIndex := 0

	for i := 0; i < int(e.ColumnCount); i++ {
		/*
		   Note: need to read partial bit before reading cols_bitmap, since
		   the partial_bits bitmap has a bit for every JSON column
		   regardless of whether it is included in the bitmap or not.
		*/
		isPartial := isPartialJsonUpdate &&
			(rowImageType == EnumRowImageTypeUpdateAI) &&
			(e.Table.ColumnType[i] == mysql.MYSQL_TYPE_JSON) &&
			isBitSetIncr(partialBitmap, &partialBitmapIndex)

		if !isBitSet(bitmap, i) {
			skips = append(skips, i)
			continue
		}

		if isBitSetIncr(nullBitmap, &nullBitmapIndex) {
			row[i] = nil
			continue
		}

		var n int
		var err error
		row[i], n, err = e.decodeValue(data[pos:], e.Table.ColumnType[i], e.Table.ColumnMeta[i], isPartial)
		if err != nil {
			return 0, err
		}
		pos += n
	}

	e.Rows = append(e.Rows, row)
	e.SkippedColumns = append(e.SkippedColumns, skips)
	return pos, nil
}

func (e *RowsEvent) parseFracTime(t interface{}) interface{} {
	v, ok := t.(fracTime)
	if !ok {
		return t
	}

	if !e.parseTime {
		// Don't parse time, return string directly
		return v.String()
	}

	// return Golang time directly
	return v.Time
}

// see mysql sql/log_event.cc log_event_print_value
func (e *RowsEvent) decodeValue(data []byte, tp byte, meta uint16, isPartial bool) (v interface{}, n int, err error) {
	length := 0

	if tp == mysql.MYSQL_TYPE_STRING {
		if meta >= 256 {
			b0 := uint8(meta >> 8)
			b1 := uint8(meta & 0xFF)

			if b0&0x30 != 0x30 {
				length = int(uint16(b1) | (uint16((b0&0x30)^0x30) << 4))
				tp = b0 | 0x30
			} else {
				length = int(meta & 0xFF)
				tp = b0
			}
		} else {
			length = int(meta)
		}
	}

	switch tp {
	case mysql.MYSQL_TYPE_NULL:
		return nil, 0, nil
	case mysql.MYSQL_TYPE_LONG:
		n = 4
		v = mysql.ParseBinaryInt32(data)
	case mysql.MYSQL_TYPE_TINY:
		n = 1
		v = mysql.ParseBinaryInt8(data)
	case mysql.MYSQL_TYPE_SHORT:
		n = 2
		v = mysql.ParseBinaryInt16(data)
	case mysql.MYSQL_TYPE_INT24:
		n = 3
		v = mysql.ParseBinaryInt24(data)
	case mysql.MYSQL_TYPE_LONGLONG:
		n = 8
		v = mysql.ParseBinaryInt64(data)
	case mysql.MYSQL_TYPE_NEWDECIMAL:
		prec := uint8(meta >> 8)
		scale := uint8(meta & 0xFF)
		v, n, err = decodeDecimal(data, int(prec), int(scale), e.useDecimal)
	case mysql.MYSQL_TYPE_FLOAT:
		n = 4
		v = mysql.ParseBinaryFloat32(data)
	case mysql.MYSQL_TYPE_DOUBLE:
		n = 8
		v = mysql.ParseBinaryFloat64(data)
	case mysql.MYSQL_TYPE_BIT:
		nbits := ((meta >> 8) * 8) + (meta & 0xFF)
		n = int(nbits+7) / 8

		// use int64 for bit
		v, err = decodeBit(data, int(nbits), n)
	case mysql.MYSQL_TYPE_TIMESTAMP:
		n = 4
		t := binary.LittleEndian.Uint32(data)
		if t == 0 {
			v = "0000-00-00 00:00:00"
		} else {
			v = e.parseFracTime(fracTime{
				Time:                    time.Unix(int64(t), 0),
				Dec:                     0,
				timestampStringLocation: e.timestampStringLocation,
			})
		}
	case mysql.MYSQL_TYPE_TIMESTAMP2:
		v, n, err = decodeTimestamp2(data, meta, e.timestampStringLocation)
		v = e.parseFracTime(v)
	case mysql.MYSQL_TYPE_DATETIME:
		n = 8
		i64 := binary.LittleEndian.Uint64(data)
		if i64 == 0 {
			v = "0000-00-00 00:00:00"
		} else {
			d := i64 / 1000000
			t := i64 % 1000000
			years := int(d / 10000)
			months := int(d%10000) / 100
			days := int(d % 100)
			hours := int(t / 10000)
			minutes := int(t%10000) / 100
			seconds := int(t % 100)
			if !e.parseTime || months == 0 || days == 0 {
				v = fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d",
					years, months, days, hours, minutes, seconds)
			} else {
				v = e.parseFracTime(fracTime{
					Time: time.Date(
						years,
						time.Month(months),
						days,
						hours,
						minutes,
						seconds,
						0,
						time.UTC,
					),
					Dec: 0,
				})
			}
		}
	case mysql.MYSQL_TYPE_DATETIME2:
		v, n, err = decodeDatetime2(data, meta, e.parseTime)
		v = e.parseFracTime(v)
	case mysql.MYSQL_TYPE_TIME:
		n = 3
		i32 := uint32(mysql.FixedLengthInt(data[0:3]))
		if i32 == 0 {
			v = "00:00:00"
		} else {
			v = fmt.Sprintf("%02d:%02d:%02d", i32/10000, (i32%10000)/100, i32%100)
		}
	case mysql.MYSQL_TYPE_TIME2:
		v, n, err = decodeTime2(data, meta)
	case mysql.MYSQL_TYPE_DATE:
		n = 3
		i32 := uint32(mysql.FixedLengthInt(data[0:3]))
		if i32 == 0 {
			v = "0000-00-00"
		} else {
			v = fmt.Sprintf("%04d-%02d-%02d", i32/(16*32), i32/32%16, i32%32)
		}

	case mysql.MYSQL_TYPE_YEAR:
		n = 1
		year := int(data[0])
		if year == 0 {
			v = year
		} else {
			v = year + 1900
		}
	case mysql.MYSQL_TYPE_ENUM:
		l := meta & 0xFF
		switch l {
		case 1:
			v = int64(data[0])
			n = 1
		case 2:
			v = int64(binary.LittleEndian.Uint16(data))
			n = 2
		default:
			err = fmt.Errorf("unknown ENUM packlen=%d", l)
		}
	case mysql.MYSQL_TYPE_SET:
		n = int(meta & 0xFF)
		nbits := n * 8

		v, err = littleDecodeBit(data, nbits, n)
	case mysql.MYSQL_TYPE_BLOB:
		v, n, err = decodeBlob(data, meta)
	case mysql.MYSQL_TYPE_VARCHAR,
		mysql.MYSQL_TYPE_VAR_STRING:
		length = int(meta)
		v, n = decodeString(data, length)
	case mysql.MYSQL_TYPE_STRING:
		v, n = decodeString(data, length)
	case mysql.MYSQL_TYPE_JSON:
		// Refer: https://github.com/shyiko/mysql-binlog-connector-java/blob/master/src/main/java/com/github/shyiko/mysql/binlog/event/deserialization/AbstractRowsEventDataDeserializer.java#L404
		length = int(mysql.FixedLengthInt(data[0:meta]))
		n = length + int(meta)

		/*
		   See https://github.com/mysql/mysql-server/blob/7b6fb0753b428537410f5b1b8dc60e5ccabc9f70/sql-common/json_binary.cc#L1077

		   Each document should start with a one-byte type specifier, so an
		   empty document is invalid according to the format specification.
		   Empty documents may appear due to inserts using the IGNORE keyword
		   or with non-strict SQL mode, which will insert an empty string if
		   the value NULL is inserted into a NOT NULL column. We choose to
		   interpret empty values as the JSON null literal.

		   In our implementation (go-mysql) for backward compatibility we prefer return empty slice.
		*/
		if length == 0 {
			v = []byte{}
		} else {
			if isPartial {
				var diff *JsonDiff
				diff, err = e.decodeJsonPartialBinary(data[meta:n])
				if err == nil {
					v = diff
				} else {
					fmt.Printf("decodeJsonPartialBinary(%q) fail: %s\n", data[meta:n], err)
				}
			} else {
				var d []byte
				d, err = e.decodeJsonBinary(data[meta:n])
				if err == nil {
					v = utils.ByteSliceToString(d)
				}
			}
		}
	case mysql.MYSQL_TYPE_GEOMETRY:
		// MySQL saves Geometry as Blob in binlog
		// Seem that the binary format is SRID (4 bytes) + WKB, outer can use
		// MySQL GeoFromWKB or others to create the geometry data.
		// Refer https://dev.mysql.com/doc/refman/5.7/en/gis-wkb-functions.html
		// I also find some go libs to handle WKB if possible
		// see https://github.com/twpayne/go-geom or https://github.com/paulmach/go.geo
		v, n, err = decodeBlob(data, meta)
	case mysql.MYSQL_TYPE_VECTOR:
		v, n, err = decodeBlob(data, meta)
	default:
		err = fmt.Errorf("unsupport type %d in binlog and don't know how to handle", tp)
	}

	return v, n, err
}

func decodeString(data []byte, length int) (v string, n int) {
	if length < 256 {
		length = int(data[0])

		n = length + 1
		v = utils.ByteSliceToString(data[1:n])
	} else {
		length = int(binary.LittleEndian.Uint16(data[0:]))
		n = length + 2
		v = utils.ByteSliceToString(data[2:n])
	}

	return
}

// ref: https://github.com/mysql/mysql-server/blob/a9b0c712de3509d8d08d3ba385d41a4df6348775/strings/decimal.c#L137
const digitsPerInteger int = 9

var compressedBytes = []int{0, 1, 1, 2, 2, 3, 3, 4, 4, 4}

func decodeDecimalDecompressValue(compIndx int, data []byte, mask uint8) (size int, value uint32) {
	size = compressedBytes[compIndx]
	switch size {
	case 0:
	case 1:
		value = uint32(data[0] ^ mask)
	case 2:
		value = uint32(data[1]^mask) | uint32(data[0]^mask)<<8
	case 3:
		value = uint32(data[2]^mask) | uint32(data[1]^mask)<<8 | uint32(data[0]^mask)<<16
	case 4:
		value = uint32(data[3]^mask) | uint32(data[2]^mask)<<8 | uint32(data[1]^mask)<<16 | uint32(data[0]^mask)<<24
	}
	return
}

var zeros = [digitsPerInteger]byte{48, 48, 48, 48, 48, 48, 48, 48, 48}

func decodeDecimal(data []byte, precision int, decimals int, useDecimal bool) (interface{}, int, error) {
	// see python mysql replication and https://github.com/jeremycole/mysql_binlog
	integral := precision - decimals
	uncompIntegral := integral / digitsPerInteger
	uncompFractional := decimals / digitsPerInteger
	compIntegral := integral - (uncompIntegral * digitsPerInteger)
	compFractional := decimals - (uncompFractional * digitsPerInteger)

	binSize := uncompIntegral*4 + compressedBytes[compIntegral] +
		uncompFractional*4 + compressedBytes[compFractional]

	buf := make([]byte, binSize)
	copy(buf, data[:binSize])

	// must copy the data for later change
	data = buf

	// Support negative
	// The sign is encoded in the high bit of the the byte
	// But this bit can also be used in the value
	value := uint32(data[0])
	var res strings.Builder
	res.Grow(precision + 2)
	var mask uint32 = 0
	if value&0x80 == 0 {
		mask = uint32((1 << 32) - 1)
		res.WriteString("-")
	}

	// clear sign
	data[0] ^= 0x80

	zeroLeading := true

	pos, value := decodeDecimalDecompressValue(compIntegral, data, uint8(mask))
	if value != 0 {
		zeroLeading = false
		res.WriteString(strconv.FormatUint(uint64(value), 10))
	}

	for i := 0; i < uncompIntegral; i++ {
		value = binary.BigEndian.Uint32(data[pos:]) ^ mask
		pos += 4
		if zeroLeading {
			if value != 0 {
				zeroLeading = false
				res.WriteString(strconv.FormatUint(uint64(value), 10))
			}
		} else {
			toWrite := strconv.FormatUint(uint64(value), 10)
			res.Write(zeros[:digitsPerInteger-len(toWrite)])
			res.WriteString(toWrite)
		}
	}

	if zeroLeading {
		res.WriteString("0")
	}

	if pos < len(data) {
		res.WriteString(".")

		for i := 0; i < uncompFractional; i++ {
			value = binary.BigEndian.Uint32(data[pos:]) ^ mask
			pos += 4
			toWrite := strconv.FormatUint(uint64(value), 10)
			res.Write(zeros[:digitsPerInteger-len(toWrite)])
			res.WriteString(toWrite)
		}

		if size, value := decodeDecimalDecompressValue(compFractional, data[pos:], uint8(mask)); size > 0 {
			toWrite := strconv.FormatUint(uint64(value), 10)
			padding := compFractional - len(toWrite)
			if padding > 0 {
				res.Write(zeros[:padding])
			}
			res.WriteString(toWrite)
			pos += size
		}
	}

	if useDecimal {
		f, err := decimal.NewFromString(res.String())
		return f, pos, err
	}

	return res.String(), pos, nil
}

func decodeBit(data []byte, nbits int, length int) (value int64, err error) {
	if nbits > 1 {
		switch length {
		case 1:
			value = int64(data[0])
		case 2:
			value = int64(binary.BigEndian.Uint16(data))
		case 3:
			value = int64(mysql.BFixedLengthInt(data[0:3]))
		case 4:
			value = int64(binary.BigEndian.Uint32(data))
		case 5:
			value = int64(mysql.BFixedLengthInt(data[0:5]))
		case 6:
			value = int64(mysql.BFixedLengthInt(data[0:6]))
		case 7:
			value = int64(mysql.BFixedLengthInt(data[0:7]))
		case 8:
			value = int64(binary.BigEndian.Uint64(data))
		default:
			err = fmt.Errorf("invalid bit length %d", length)
		}
	} else {
		if length != 1 {
			err = fmt.Errorf("invalid bit length %d", length)
		} else {
			value = int64(data[0])
		}
	}
	return
}

func littleDecodeBit(data []byte, nbits int, length int) (value int64, err error) {
	if nbits > 1 {
		switch length {
		case 1:
			value = int64(data[0])
		case 2:
			value = int64(binary.LittleEndian.Uint16(data))
		case 3:
			value = int64(mysql.FixedLengthInt(data[0:3]))
		case 4:
			value = int64(binary.LittleEndian.Uint32(data))
		case 5:
			value = int64(mysql.FixedLengthInt(data[0:5]))
		case 6:
			value = int64(mysql.FixedLengthInt(data[0:6]))
		case 7:
			value = int64(mysql.FixedLengthInt(data[0:7]))
		case 8:
			value = int64(binary.LittleEndian.Uint64(data))
		default:
			err = fmt.Errorf("invalid bit length %d", length)
		}
	} else {
		if length != 1 {
			err = fmt.Errorf("invalid bit length %d", length)
		} else {
			value = int64(data[0])
		}
	}
	return
}

func decodeTimestamp2(data []byte, dec uint16, timestampStringLocation *time.Location) (interface{}, int, error) {
	// get timestamp binary length
	n := int(4 + (dec+1)/2)
	sec := int64(binary.BigEndian.Uint32(data[0:4]))
	usec := int64(0)
	switch dec {
	case 1, 2:
		usec = int64(data[4]) * 10000
	case 3, 4:
		usec = int64(binary.BigEndian.Uint16(data[4:])) * 100
	case 5, 6:
		usec = int64(mysql.BFixedLengthInt(data[4:7]))
	}

	if sec == 0 {
		return formatZeroTime(int(usec), int(dec)), n, nil
	}

	return fracTime{
		Time:                    time.Unix(sec, usec*1000),
		Dec:                     int(dec),
		timestampStringLocation: timestampStringLocation,
	}, n, nil
}

const DATETIMEF_INT_OFS int64 = 0x8000000000

func decodeDatetime2(data []byte, dec uint16, parseTime bool) (interface{}, int, error) {
	// get datetime binary length
	n := int(5 + (dec+1)/2)

	intPart := int64(mysql.BFixedLengthInt(data[0:5])) - DATETIMEF_INT_OFS
	var frac int64 = 0

	switch dec {
	case 1, 2:
		frac = int64(data[5]) * 10000
	case 3, 4:
		frac = int64(binary.BigEndian.Uint16(data[5:7])) * 100
	case 5, 6:
		frac = int64(mysql.BFixedLengthInt(data[5:8]))
	}

	if intPart == 0 {
		return formatZeroTime(int(frac), int(dec)), n, nil
	}

	tmp := intPart<<24 + frac
	// handle sign???
	if tmp < 0 {
		tmp = -tmp
	}

	// var secPart int64 = tmp % (1 << 24)
	ymdhms := tmp >> 24

	ymd := ymdhms >> 17
	ym := ymd >> 5
	hms := ymdhms % (1 << 17)

	day := int(ymd % (1 << 5))
	month := int(ym % 13)
	year := int(ym / 13)

	second := int(hms % (1 << 6))
	minute := int((hms >> 6) % (1 << 6))
	hour := int(hms >> 12)

	// DATETIME encoding for nonfractional part after MySQL 5.6.4
	// https://dev.mysql.com/doc/internals/en/date-and-time-data-type-representation.html
	// integer value for 1970-01-01 00:00:00 is
	// year*13+month = 25611 = 0b110010000001011
	// day = 1 = 0b00001
	// hour = 0 = 0b00000
	// minute = 0 = 0b000000
	// second = 0 = 0b000000
	// integer value = 0b1100100000010110000100000000000000000 = 107420450816
	if !parseTime || intPart < 107420450816 || month == 0 || day == 0 {
		return formatDatetime(year, month, day, hour, minute, second, int(frac), int(dec)), n, nil
	}

	return fracTime{
		Time: time.Date(year, time.Month(month), day, hour, minute, second, int(frac*1000), time.UTC),
		Dec:  int(dec),
	}, n, nil
}

const (
	TIMEF_OFS     int64 = 0x800000000000
	TIMEF_INT_OFS int64 = 0x800000
)

func decodeTime2(data []byte, dec uint16) (string, int, error) {
	// time  binary length
	n := int(3 + (dec+1)/2)

	tmp := int64(0)
	intPart := int64(0)
	frac := int64(0)
	switch dec {
	case 1, 2:
		intPart = int64(mysql.BFixedLengthInt(data[0:3])) - TIMEF_INT_OFS
		frac = int64(data[3])
		if intPart < 0 && frac != 0 {
			/*
			   Negative values are stored with reverse fractional part order,
			   for binary sort compatibility.

			     Disk value  intpart frac   Time value   Memory value
			     800000.00    0      0      00:00:00.00  0000000000.000000
			     7FFFFF.FF   -1      255   -00:00:00.01  FFFFFFFFFF.FFD8F0
			     7FFFFF.9D   -1      99    -00:00:00.99  FFFFFFFFFF.F0E4D0
			     7FFFFF.00   -1      0     -00:00:01.00  FFFFFFFFFF.000000
			     7FFFFE.FF   -1      255   -00:00:01.01  FFFFFFFFFE.FFD8F0
			     7FFFFE.F6   -2      246   -00:00:01.10  FFFFFFFFFE.FE7960

			     Formula to convert fractional part from disk format
			     (now stored in "frac" variable) to absolute value: "0x100 - frac".
			     To reconstruct in-memory value, we shift
			     to the next integer value and then substruct fractional part.
			*/
			intPart++     /* Shift to the next integer value */
			frac -= 0x100 /* -(0x100 - frac) */
		}
		tmp = intPart<<24 + frac*10000
	case 3, 4:
		intPart = int64(mysql.BFixedLengthInt(data[0:3])) - TIMEF_INT_OFS
		frac = int64(binary.BigEndian.Uint16(data[3:5]))
		if intPart < 0 && frac != 0 {
			/*
			   Fix reverse fractional part order: "0x10000 - frac".
			   See comments for FSP=1 and FSP=2 above.
			*/
			intPart++       /* Shift to the next integer value */
			frac -= 0x10000 /* -(0x10000-frac) */
		}
		tmp = intPart<<24 + frac*100

	case 5, 6:
		tmp = int64(mysql.BFixedLengthInt(data[0:6])) - TIMEF_OFS
		return timeFormat(tmp, dec, n)
	default:
		intPart = int64(mysql.BFixedLengthInt(data[0:3])) - TIMEF_INT_OFS
		tmp = intPart << 24
	}

	if intPart == 0 && frac == 0 {
		return "00:00:00", n, nil
	}

	return timeFormat(tmp, dec, n)
}

func timeFormat(tmp int64, dec uint16, n int) (string, int, error) {
	hms := int64(0)
	sign := ""
	if tmp < 0 {
		tmp = -tmp
		sign = "-"
	}

	hms = tmp >> 24

	hour := (hms >> 12) % (1 << 10) /* 10 bits starting at 12th */
	minute := (hms >> 6) % (1 << 6) /* 6 bits starting at 6th   */
	second := hms % (1 << 6)        /* 6 bits starting at 0th   */
	secPart := tmp % (1 << 24)

	if secPart != 0 {
		s := fmt.Sprintf("%s%02d:%02d:%02d.%06d", sign, hour, minute, second, secPart)
		return s[0 : len(s)-(6-int(dec))], n, nil
	}

	return fmt.Sprintf("%s%02d:%02d:%02d", sign, hour, minute, second), n, nil
}

func decodeBlob(data []byte, meta uint16) (v []byte, n int, err error) {
	var length int
	switch meta {
	case 1:
		length = int(data[0])
		v = data[1 : 1+length]
		n = length + 1
	case 2:
		length = int(binary.LittleEndian.Uint16(data))
		v = data[2 : 2+length]
		n = length + 2
	case 3:
		length = int(mysql.FixedLengthInt(data[0:3]))
		v = data[3 : 3+length]
		n = length + 3
	case 4:
		length = int(binary.LittleEndian.Uint32(data))
		v = data[4 : 4+length]
		n = length + 4
	default:
		err = fmt.Errorf("invalid blob packlen = %d", meta)
	}

	return
}

func (e *RowsEvent) Dump(w io.Writer) {
	fmt.Fprintf(w, "TableID: %d\n", e.TableID)
	fmt.Fprintf(w, "Flags: %d\n", e.Flags)
	fmt.Fprintf(w, "Column count: %d\n", e.ColumnCount)
	fmt.Fprintf(w, "NDB data: %s\n", e.NdbData)
	fmt.Fprintf(w, "Event type: %s (%s)", e.Type(), e.eventType)

	fmt.Fprintf(w, "Values:\n")
	for _, rows := range e.Rows {
		fmt.Fprintf(w, "--\n")
		for j, d := range rows {
			switch dt := d.(type) {
			case []byte:
				fmt.Fprintf(w, "%d:%q\n", j, dt)
			case *JsonDiff:
				fmt.Fprintf(w, "%d:%s\n", j, dt)
			default:
				fmt.Fprintf(w, "%d:%#v\n", j, d)
			}
		}
	}
	fmt.Fprintln(w)
}

type RowsQueryEvent struct {
	Query []byte
}

func (e *RowsQueryEvent) Decode(data []byte) error {
	// ignore length byte 1
	e.Query = data[1:]
	return nil
}

func (e *RowsQueryEvent) Dump(w io.Writer) {
	fmt.Fprintf(w, "Query: %s\n", e.Query)
	fmt.Fprintln(w)
}
