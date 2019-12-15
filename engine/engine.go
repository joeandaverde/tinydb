package engine

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"github.com/joeandaverde/tinydb/ast"
	log "github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/joeandaverde/tinydb/internal/btree"
)

type (
	// ColumnDefinition represents a specification for a column in a table
	ColumnDefinition struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		Offset     int    `json:"offset"`
		PrimaryKey bool   `json:"is_primary_key"`
	}

	TableDefinition struct {
		Name    string             `json:"name"`
		Columns []ColumnDefinition `json:"columns"`
	}

	indexedField struct {
		value  string
		offset int
	}

	pkJob struct {
		table      TableDefinition
		fieldIndex int
		result     *btree.BTree
	}

	// Config describes the configuration for the database
	Config struct {
		DataDir string `yaml:"data_directory"`
		Addr    string `yaml:"listen"`
	}

	// Engine holds metadata and indexes about the database
	Engine struct {
		Indexes   map[string]*btree.BTree
		Tables    map[string]TableDefinition
		Log       *log.Logger
		Config    *Config
		adminLock sync.Mutex
	}

	ExecutionEnvironment struct {
		ColumnLookup map[string]ColumnDefinition
		Tables       map[string]TableDefinition
		Columns      []string
		Indexes      map[string]*btree.BTree
		Engine       *Engine
	}
)

func (f *indexedField) Less(than btree.Item) bool {
	return f.value < than.(*indexedField).value
}

// Start initializes a new TinyDb database engine
func Start(basePath string) *Engine {
	log.Infof("Starting database engine [DataDir: %s]", basePath)

	config := &Config{
		DataDir: basePath,
	}

	tables := loadTableDefinitions(config)
	indexes := buildIndexes(config, tables)
	logger := log.New()

	return &Engine{
		Tables:  tables,
		Indexes: indexes,
		Config:  config,
		Log:     logger,
	}
}

// Execute runs a statement against the database engine
func (e *Engine) Execute(text string) (*ResultSet, error) {
	startingTime := time.Now().UTC()
	defer func() {
		duration := time.Now().UTC().Sub(startingTime)
		e.Log.Infof("\nDuration: %s\n", duration)
	}()

	e.Log.Debug("EXEC: ", text)

	statement, err := ast.Parse(strings.TrimSpace(text))
	if err != nil {
		return nil, err
	}

	return executeStatement(e, statement)
}

func (e *Engine) loadTables() {
	newTables := loadTableDefinitions(e.Config)
	e.adminLock.Lock()
	e.Tables = newTables
	e.adminLock.Unlock()
}

func executeStatement(engine *Engine, statement ast.Statement) (*ResultSet, error) {
	switch s := (statement).(type) {
	case *ast.CreateTableStatement:
		if _, err := createTable(engine, s); err != nil {
			return nil, err
		}
		engine.loadTables()
		return EmptyResultSet(), nil
	case *ast.InsertStatement:
		_, result, err := doInsert(engine, s)

		if err != nil {
			return nil, err
		}

		return result, nil
	case *ast.SelectStatement:
		return doSelect(engine, s)
	default:
		return nil, fmt.Errorf("unexpected statement type")
	}
}

func newTableScanner(config *Config, tableName string) (*csv.Reader, error) {
	csvFile, err := os.Open(filepath.Join(config.DataDir, tableName, "data.csv"))

	if err != nil {
		return nil, err
	}

	tableCsv := csv.NewReader(bufio.NewReader(csvFile))

	return tableCsv, nil
}

func buildIndex(config *Config, job *pkJob) {
	btree := btree.New(5)

	csvReader, err := newTableScanner(config, job.table.Name)

	if err != nil {
		panic("unable to build index")
	}

	rowCount := 0
	for {
		data, err := csvReader.Read()

		if err == io.EOF {
			break
		} else if err != nil {
			log.Fatal(err)
		}

		rowCount++

		btree.Insert(&indexedField{
			value:  data[job.fieldIndex],
			offset: rowCount,
		})
	}

	job.result = btree
}

func buildIndexes(config *Config, m map[string]TableDefinition) map[string]*btree.BTree {
	indexes := make(map[string]*btree.BTree)
	results := make(chan *pkJob)

	var wg sync.WaitGroup

	for _, t := range m {
		for i, c := range t.Columns {
			if c.PrimaryKey {
				wg.Add(1)
				go func(i int, t TableDefinition) {
					defer wg.Done()
					job := &pkJob{fieldIndex: i, table: t}
					buildIndex(config, job)
					results <- job
				}(i, t)
			}
		}
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for job := range results {
		indexes[job.table.Name] = job.result
	}

	return indexes
}

func loadTableDefinitions(config *Config) map[string]TableDefinition {
	tableDefinitions := make(map[string]TableDefinition)

	filepath.Walk(config.DataDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if strings.HasSuffix(p, "metadata.json") {
			data, err := ioutil.ReadFile(p)

			if err != nil {
				panic("unable to load tables")
			}

			var tableDefinition TableDefinition
			err = json.Unmarshal(data, &tableDefinition)

			if err != nil {
				panic("unable to load tables")
			}

			tableDefinitions[tableDefinition.Name] = tableDefinition
		}

		return nil
	})

	return tableDefinitions
}

func newExecutionEnvironment(engine *Engine, tables []ast.TableAlias) (*ExecutionEnvironment, error) {
	columnLookup := make(map[string]ColumnDefinition)
	tableMetadata := make(map[string]TableDefinition)
	allMetadata := make([]TableDefinition, len(tables))

	for _, tableAlias := range tables {
		metadata, _ := engine.Tables[tableAlias.Name]

		for _, c := range metadata.Columns {
			columnLookup[fmt.Sprintf("%s.%s", tableAlias.Alias, c.Name)] = c
		}

		tableMetadata[tableAlias.Alias] = metadata
		allMetadata = append(allMetadata, metadata)
	}

	return &ExecutionEnvironment{
		Tables:       tableMetadata,
		ColumnLookup: columnLookup,
		Engine:       engine,
	}, nil
}
