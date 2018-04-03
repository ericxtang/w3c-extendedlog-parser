package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx"
	"github.com/jackc/pgx/pgio"
	"github.com/jackc/pgx/pgtype"
	unidecode "github.com/mozillazg/go-unidecode"
	uuid "github.com/satori/go.uuid"
	"github.com/spf13/cobra"
	parser "github.com/stephane-martin/w3c-extendedlog-parser"
	"golang.org/x/text/encoding/charmap"
)

const (
	usecsPerHour    = 3600000000
	usecsPerMinute  = 60000000
	usecsPerSec     = 1000000
	nanosecsPerUsec = 1000
)

var parallel uint8
var batchsize int

var isoDecoder = charmap.ISO8859_15.NewDecoder()

var push2pgCmd = &cobra.Command{
	Use:   "push2pg",
	Short: "Parse accesslog files and push events to postgres",
	Run: func(cmd *cobra.Command, args []string) {
		if parallel == 0 {
			parallel = 1
		}
		if batchsize == 0 {
			batchsize = 5000
		}
		if len(filenames) == 0 {
			fatal(errors.New("specify the files to be parsed"))
		}
		dbURI = strings.TrimSpace(dbURI)
		if len(dbURI) == 0 {
			fatal(errors.New("Empty uri"))
		}
		config, err := pgx.ParseConnectionString(dbURI)
		fatal(err)
		pool, err := pgx.NewConnPool(pgx.ConnPoolConfig{
			ConnConfig:     config,
			MaxConnections: int(parallel),
		})
		fatal(err)
		defer pool.Close()
		uploadFilesPG(filenames, pool, uint(parallel), batchsize)
	},
}

func uploadFilesPG(fnames []string, pool *pgx.ConnPool, nbInjectors uint, bsize int) {
	fnamesChan := make(chan string)
	var wg sync.WaitGroup

	for i := uint(0); i < nbInjectors; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				fname, ok := <-fnamesChan
				if !ok {
					return
				}
				uploadFilePG(fname, pool, bsize)
			}
		}()
	}

	for _, fname := range fnames {
		fnamesChan <- fname
	}
	close(fnamesChan)
	wg.Wait()
}

func uploadFilePG(fname string, pool *pgx.ConnPool, bsize int) {
	fname = strings.TrimSpace(fname)
	f, err := os.Open(fname)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening '%s': %s\n", fname, err)
		return
	}

	fmt.Fprintln(os.Stderr, "Uploading:", fname)
	start := time.Now()
	nbLines, err := uploadPG(f, pool, bsize)
	duration := time.Now().Sub(start).Seconds()
	f.Close()
	if err == nil {
		fmt.Fprintf(
			os.Stderr,
			"Successfully uploaded: %s (%d lines, %f secs, %d lines/sec)\n",
			fname, nbLines, duration, int(float64(nbLines)/duration),
		)
	} else {
		fmt.Fprintf(os.Stderr, "Error uploading '%s': %s\n", fname, err)
	}
}

type Row []interface{}

func (r *Row) AddField(field interface{}) error {
	if len(*r) < cap(*r) {
		*r = append(*r, field)
		return nil
	}
	return fmt.Errorf("too many fields (max %d)", cap(*r))
}

type Rows struct {
	pool     *sync.Pool
	maxSize  int
	nbFields int
	rows     []*Row
}

func RowFactory(maxSize int, nbFields int) *Rows {
	r := Rows{
		maxSize:  maxSize,
		nbFields: nbFields,
		pool: &sync.Pool{
			New: func() interface{} {
				return Row(make([]interface{}, 0, nbFields))
			},
		},
		rows: make([]*Row, 0, maxSize),
	}
	return &r
}

func (r *Rows) GetRow() (*Row, bool) {
	if len(r.rows) < r.maxSize {
		row := r.pool.Get().(Row)
		row = row[:0]
		r.rows = append(r.rows, &row)
		return &row, false
	}
	return nil, true
}

func (r *Rows) GetSource() (s *Source, err error) {
	for i, row := range r.rows {
		if len(*row) != r.nbFields {
			return nil, fmt.Errorf("wrong number of fields (for line %d, expected %d, got %d)", i, r.nbFields, len(*row))
		}
	}
	return &Source{r: r}, nil
}

func (r *Rows) String() string {
	buf := bytes.NewBuffer(nil)
	for _, row := range r.rows {
		for _, field := range *row {
			buf.WriteString(fmt.Sprintf("%v ", field))
		}
		buf.WriteString("\n")
	}
	return buf.String()
}

func (r *Rows) Clear() {
	var row *Row
	for _, row = range r.rows {
		r.pool.Put(*row)
	}
	r.rows = r.rows[:0]
}

func (r *Rows) Len() int {
	return len(r.rows)
}

type Source struct {
	r   *Rows
	idx int
}

func (s *Source) Next() bool {
	s.idx++
	return s.idx < s.r.Len()
}

func (s *Source) Values() ([]interface{}, error) {
	return ([]interface{})(*(s.r.rows[s.idx])), nil
}

func (s *Source) Err() error {
	return nil
}

func uploadPG(f io.Reader, connPool *pgx.ConnPool, bsize int) (nbLines int, err error) {
	p := parser.NewFileParser(f)
	err = p.ParseHeader()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error building parser:", err)
		return 0, err
	}
	curFieldNames := p.FieldNames()
	if !p.HasGmtTime() {
		curFieldNames = append([]string{"gmttime"}, curFieldNames...)
	}
	curFieldNames = append([]string{"id"}, curFieldNames...)
	nbFields := len(curFieldNames)

	columnNames := make([]string, 0, nbFields)
	types := make(map[string]parser.Kind, nbFields)
	for _, name := range curFieldNames {
		// make sure column names are PG compatible
		columnNames = append(columnNames, pgKey(name))
		// store the data type for each column
		types[name] = parser.GuessType(name)
	}

	conn, err := connPool.Acquire()
	if err != nil {
		return 0, err
	}
	defer connPool.Release(conn)

	factory := RowFactory(bsize, nbFields)

	uploadRows := func() error {
		if factory.Len() == 0 {
			return nil
		}
		s, err := factory.GetSource()
		if err != nil {
			return err
		}
		_, err = conn.CopyFrom(pgx.Identifier{tableName}, columnNames, s)
		if err != nil {
			return err
		}
		factory.Clear()
		return nil
	}

	var full bool
	var row *Row
	var line *parser.Line

	for {
		line, err = p.NextTo(line)
		if line == nil || err != nil {
			break
		}

		row, full = factory.GetRow()
		if full {
			// we have batchsize lines, let's flush
			err = uploadRows()
			if err != nil {
				return 0, err
			}
			row, _ = factory.GetRow()
		}

		nbLines++
		for _, name := range curFieldNames {
			if name == "id" {
				uuid, err := uuid.NewV1()
				if err != nil {
					return 0, err
				}
				err = row.AddField(uuid.Bytes())
				if err != nil {
					return 0, err
				}
			} else {
				// append converted type
				err = row.AddField(pgConvert(types[name], line.Get(name)))
				if err != nil {
					return 0, err
				}
			}
		}
	}

	// push remaining lines
	err = uploadRows()
	if err != nil {
		return 0, err
	}

	_, err = conn.Exec("VACUUM;")
	return nbLines, err
}

// MyMyTime encapsulates parser.Time so that it can be serialized to PG.
//
// We don't implement EncodeBinary on MyTime to avoid a dependancy on pgio
// in the library part.
type MyMyTime struct {
	parser.Time
}

// EncodeBinary implements the MarshalBinary interface.
func (src *MyMyTime) EncodeBinary(ci *pgtype.ConnInfo, buf []byte) ([]byte, error) {
	if src == nil {
		return nil, nil
	}
	return pgio.AppendInt64(
		buf,
		(int64(src.Hour)*usecsPerHour)+(int64(src.Minute)*usecsPerMinute)+(int64(src.Second)*usecsPerSec)+(int64(src.Nanosecond)/nanosecsPerUsec),
	), nil
}

func pgDefaultVal(t parser.Kind) interface{} {
	switch t {
	case parser.MyDate:
		return &pgtype.Date{Status: pgtype.Null}
	case parser.MyIP:
		return &pgtype.Inet{Status: pgtype.Null}
	case parser.MyTime:
		var timePtr *MyMyTime
		return timePtr
	case parser.MyTimestamp:
		return &pgtype.Timestamptz{Status: pgtype.Null}
	case parser.MyURI:
		return ""
	case parser.Float64:
		return &pgtype.Float8{Status: pgtype.Null}
	case parser.Int64:
		return &pgtype.Int8{Status: pgtype.Null}
	case parser.Bool:
		return &pgtype.Bool{Status: pgtype.Null}
	case parser.String:
		return ""
	}
	return ""
}

func pgConvert(t parser.Kind, value interface{}) interface{} {
	if value == nil {
		return pgDefaultVal(t)
	}
	switch t {
	case parser.MyDate:
		v := value.(parser.Date)
		if v.IsZero() {
			return pgDefaultVal(t)
		}
		return time.Date(v.Year, v.Month, v.Day, 0, 0, 0, 0, time.UTC)
	case parser.MyIP:
		inet := &pgtype.Inet{}
		inet.Set(value.(net.IP))
		return inet
	case parser.MyTime:
		v := value.(parser.Time)
		if v.IsZero() {
			return pgDefaultVal(t)
		}
		return &MyMyTime{Time: v}
	case parser.MyTimestamp:
		v := value.(time.Time)
		if v.IsZero() {
			return pgDefaultVal(t)
		}
		return &pgtype.Timestamptz{Status: pgtype.Present, Time: v}
	case parser.MyURI:
		return decodeCharset(value.(string))
	case parser.Float64:
		return value.(float64)
	case parser.Int64:
		return value.(int64)
	case parser.Bool:
		return value.(bool)
	case parser.String:
		return decodeCharset(value.(string))
	}
	return decodeCharset(value.(string))
}

func decodeCharset(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	utf, err := isoDecoder.String(s)
	if err == nil {
		return utf
	}
	return unidecode.Unidecode(s)
}

func init() {
	rootCmd.AddCommand(push2pgCmd)
	push2pgCmd.Flags().StringArrayVar(&filenames, "filename", []string{}, "the files to parse")
	push2pgCmd.Flags().StringVar(&tableName, "tablename", "accesslogs", "name of pg table to push events to")
	push2pgCmd.Flags().StringVar(&dbURI, "uri", "", "the URI of the postgresql server to connect to")
	push2pgCmd.Flags().Uint8Var(&parallel, "parallel", 1, "number of parallel injectors")
	push2pgCmd.Flags().IntVar(&batchsize, "batchsize", 5000, "batch size for postgresql INSERT")
}
