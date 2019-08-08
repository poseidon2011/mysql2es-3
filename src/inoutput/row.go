package inoutput

import (
	"config"
	"fmt"
	"github.com/go-ozzo/ozzo-dbx"
	"log"
	"strconv"
	"time"
)

var cfg *config.Config
var db *dbx.DB

func init() {
	cfg = config.NewConfig()
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	var err error
	db, err = dbx.Open("mysql", cfg.DB.Username+":"+cfg.DB.Password+"@/"+cfg.DB.DatabaseName)
	if err != nil {
		log.Panicln("Open database error: " + err.Error())
	}
}

type Row struct {
	Items []map[string]interface{}
}

func (r Row) Read() ([]map[string]interface{}, error) {
	items := make([]map[string]interface{}, 0)
	fmt.Println("Begin sync")
	row := dbx.NullStringMap{}
	tables := make([]string, 0)
	db.NewQuery("SHOW TABLES").Column(&tables)
	dbOptions := cfg.DBOptions
	pkName := dbOptions.DefaultPk
	pkValue := ""
	fmt.Println(pkValue)
	for _, table := range tables {
		if In(table, dbOptions.IgnoreTables) {
			continue
		}
		ignoreFields := make([]string, 0)
		datetimeFormatFields := dbOptions.DatetimeFormatFields
		for k, v := range dbOptions.Tables {
			if k == table {
				if len(v.PK) == 0 {
					pkName = dbOptions.DefaultPk
				}
				ignoreFields = v.IgnoreFields
				datetimeFormatFields = append(datetimeFormatFields, v.DatetimeFormatFields...)
				break
			}
		}
		if len(pkName) == 0 {
			pkName = dbOptions.DefaultPk
		}
		fmt.Println("Table: " + table)
		sq := db.Select().From(table).Limit(cfg.SizePerTime)
		rows, err := sq.Rows()
		if err == nil {

			for rows.Next() {
				rows.ScanMap(row)
				item := make(map[string]interface{})
				for fieldName, v := range row {
					if In(fieldName, ignoreFields) {
						continue
					}
					fieldValue, _ := v.Value()
					if fieldName == pkName {
						pkValue = v.String
					}
					if In(fieldName, datetimeFormatFields) {
						fieldName += "_formatted"
						v, _ := strconv.ParseInt(fieldValue.(string), 10, 64)
						fieldValue = time.Unix(v, 0)
					}
					item[fieldName] = fieldValue
				}
				if cfg.Debug {
					fmt.Println(fmt.Sprintf("#%v", item))
				}
				items = append(items, item)
			}
		}
	}

	r.Items = items
	return items, nil
}

func (r Row) Write() error {
	for _, v := range r.Items {
		fmt.Println(v)
	}

	return nil
}
