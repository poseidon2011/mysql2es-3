package inoutput

import (
	"config"
	"context"
	"database/sql"
	"fmt"
	"github.com/go-ozzo/ozzo-dbx"
	"github.com/olivere/elastic"
	"log"
	"m2elog"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"
)

var cfg *config.Config
var db *dbx.DB
var esClient *elastic.Client
var ctx context.Context

const (
	IntFieldType      = "int"
	StringFieldType   = "string"
	DateFieldType     = "date"
	TimeFieldType     = "time"
	DatetimeFieldType = "datetime"
)

func init() {
	cfg = config.NewConfig()
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	var err error
	db, err = dbx.Open("mysql", cfg.DB.Username+":"+cfg.DB.Password+"@/"+cfg.DB.DatabaseName)
	if err != nil {
		log.Panicln("Open database error: " + err.Error())
	}

	var options []elastic.ClientOptionFunc
	options = append(options, elastic.SetSniff(false))
	if len(cfg.ES.Urls) > 0 {
		options = append(options, elastic.SetURL(cfg.ES.Urls...))
	}
	if len(cfg.ES.BaseAuth.Username) > 0 && len(cfg.ES.BaseAuth.Password) > 0 {
		options = append(options, elastic.SetBasicAuth(cfg.ES.BaseAuth.Username, cfg.ES.BaseAuth.Password))
	}
	esClient, err = elastic.NewClient(options...)
	if err != nil {
		log.Panicln(err)
	}

	ctx = context.Background()
	rand.Seed(time.Now().Unix())
}

type Row struct {
	TableIndexes map[string]string
	Items        []ESItem
}

func (r *Row) Init() (err error) {
	if r.TableIndexes == nil {
		r.TableIndexes = make(map[string]string, 0)
	}
	tables := make([]string, 0)
	dbOptions := cfg.DBOptions
	db.NewQuery("SHOW TABLES").Column(&tables)
	syncAllTables := false
	if len(dbOptions.SyncTables) == 1 && dbOptions.SyncTables[0] == "*" {
		syncAllTables = true
	}
	for _, table := range tables {
		if (syncAllTables && !In(table, dbOptions.IgnoreTables)) || In(table, dbOptions.SyncTables) {
			// 检测 ES index 是否存在
			indexName := table
			for k, v := range dbOptions.MergeTables {
				if In(table, v) {
					indexName = k
					break
				}
			}
			indexName = cfg.ES.IndexPrefix + indexName
			r.TableIndexes[table] = indexName
			exists := false
			exists, err = esClient.IndexExists(indexName).Do(ctx)
			if err != nil {
				log.Panicln(err)
			} else if !exists {
				log.Println(fmt.Sprintf("Create ES `%s` index", indexName))
				esClient.CreateIndex(indexName).Do(ctx)
			}
		}
	}

	return err
}

func (r *Row) Read() (err error) {
	dbOptions := cfg.DBOptions
	for table, indexName := range r.TableIndexes {
		row := dbx.NullStringMap{}
		pkName := ""
		var pkValue interface{}
		pkType := dbOptions.DefaultPkType
		ignoreFields := make([]string, 0)
		datetimeFormatFields := dbOptions.DatetimeFormatFields
		for k, v := range dbOptions.Tables {
			if k == table {
				if len(v.PK) > 0 {
					pkName = v.PK
				}
				if len(v.PkType) > 0 {
					pkType = v.PkType
				}
				ignoreFields = v.IgnoreFields
				datetimeFormatFields = append(datetimeFormatFields, v.DatetimeFormatFields...)
				break
			}
		}
		if len(pkName) == 0 {
			pkName = dbOptions.DefaultPk
		}
		lastId := ""
		if pkType == m2elog.PKIntType {
			db.Select("MAX(pk_int_value)").From(m2elog.TableName).Where(dbx.HashExp{"table_name": table}).Row(&lastId)
		}
		type ColumnType struct {
			Field sql.NullString `db:"Field"`
			Type  sql.NullString `db:"Type"`
		}
		tmp := make([]ColumnType, 0)
		db.NewQuery("SHOW COLUMNS FROM " + table).All(&tmp)
		tableColumnTypes := make(map[string]string, 0)
		for _, t := range tmp {
			if t.Field.Valid {
				fType := StringFieldType
				if t.Type.Valid {
					fName := t.Type.String
					index := strings.Index(fName, "(")
					if index != -1 {
						fName = fName[:index]
					}
					switch fName {
					case "int", "smallint", "tinyint":
						fType = IntFieldType
					case "date":
						fType = DateFieldType
					case "time":
						fType = TimeFieldType
					case "datetime":
						fType = DatetimeFieldType
					default:
						fType = StringFieldType
					}
				}
				tableColumnTypes[t.Field.String] = fType
			}
		}

	queryDatabase:
		sq := db.Select().From(table).Limit(cfg.SizePerTime)
		if len(lastId) > 0 {
			fmt.Println("LastId: ", lastId)
			sq.Where(dbx.NewExp(fmt.Sprintf("%s > %s", pkName, lastId)))
		}
		var rows *dbx.Rows
		rows, err = sq.Rows()
		if err == nil {
			i := 0
			for rows.Next() {
				i++
				rows.ScanMap(row)
				item := ESItem{
					TableName: table,
					IndexName: indexName,
					IdName:    pkName,
				}
				values := make(map[string]interface{})
				for fieldName, v := range row {
					if In(fieldName, ignoreFields) {
						continue
					}
					fieldValue, _ := v.Value()
					if fieldName == pkName {
						pkValue = v.String
					}
					item.IdValue = fmt.Sprintf("%s", pkValue)
					if In(fieldName, datetimeFormatFields) {
						v, _ := strconv.ParseInt(fieldValue.(string), 10, 64)
						values[fieldName+"_formatted"] = time.Unix(v, 0)
					}
					if fieldValue != nil {
						switch tableColumnTypes[fieldName] {
						case IntFieldType:
							fieldValue, _ = strconv.ParseInt(fieldValue.(string), 10, 64)
						default:
							fieldValue = fmt.Sprintf("%s", fieldValue)
						}
					} else {
						fieldValue = ""
					}
					values[fieldName] = fieldValue
				}
				item.Values = values
				lastId = item.IdValue
				r.Items = append(r.Items, item)
			}
			if i > 0 {
				goto queryDatabase
			}
		} else {
			log.Panicln(err)
		}
	}

	return
}

func (r *Row) Write() (insertCount, updateCount, deleteCount int, err error) {
	var e error
	var wg sync.WaitGroup
	indexService := esClient.Index()
	updateService := esClient.Update()
	for _, item := range r.Items {
		for {
			if tasks, err := esClient.TasksList().Do(ctx); err == nil {
				tasksCount := 0
				for _, node := range tasks.Nodes {
					tasksCount += len(node.Tasks)
				}
				if tasksCount <= 500 {
					break
				} else {
					time.Sleep(3 * time.Second)
				}
			} else {
				time.Sleep(2 * time.Second)
			}
		}
		wg.Add(1)
		go func(wg *sync.WaitGroup, item ESItem) {
			maxTimes := 3
			times := 0
			for times < maxTimes {
				if times > 0 {
					time.Sleep(time.Duration(times+rand.Intn(times)) * time.Second)
				}
				q, err := esClient.Search(item.IndexName).Query(elastic.NewTermQuery(item.IdName, item.IdValue)).Do(ctx)
				if err == nil {
					intValue, _ := strconv.ParseInt(item.IdValue, 10, 64)
					eLog := m2elog.M2ELog{
						TableName:  item.TableName,
						PkName:     item.IdName,
						PkIntValue: intValue,
					}
					if q.TotalHits() == 0 {
						put, err := indexService.
							Index(item.IndexName).
							Id(item.IdValue).
							BodyJson(item.Values).
							Do(ctx)
						if err == nil {
							eLog.Save()
							insertCount++
							log.Printf("Indexed `%s` to `%s` index, type `%s`\n", put.Id, put.Index, put.Type)
							times = maxTimes
						} else {
							log.Printf("IndexName: %s, IdName: %s, IdValue: %s, err: %v", item.IndexName, item.IdName, item.IdValue, err)
						}
					} else {
						put, err := updateService.
							Index(item.IndexName).
							Id(item.IdValue).
							Doc(item.Values).
							Do(ctx)
						if err == nil {
							eLog.Save()
							updateCount++
							log.Printf("Update `%s` to `%s` index, type `%s`\n", put.Id, put.Index, put.Type)
							times = maxTimes
						} else {
							fmt.Println(fmt.Sprintf("%#v", item.Values))
							log.Printf("IndexName: %s, IdName: %s, IdValue: %s, err: %v", item.IndexName, item.IdName, item.IdValue, err)
						}
					}
				} else {
					e = err
				}
				times++
			}

			wg.Done()
		}(&wg, item)
	}
	wg.Wait()

	return insertCount, updateCount, deleteCount, e
}
