package mysql

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
)

type Config struct {
	Name              string
	DBName            string
	User              string
	Password          string
	Host              string
	Port              int
	Charset           string
	ConnectTimeout    time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	MaxOpenConns      int
	MaxIdleConns      int
	ConnMaxLifetime   time.Duration
	ConnMaxIdleTime   time.Duration
	InterpolateParams bool
	UnitName          string
}

// DBConf is the stable connection description used by Config, DBInit and GetDB.
type DBConf struct {
	Dbname            string
	User              string
	Password          string
	Host              string
	Port              int
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	ConnectTimeout    time.Duration
	Charset           string
	MaxOpenConns      int
	MaxIdleConns      int
	ConnMaxLifeTime   time.Duration
	ConnMaxIdleTime   time.Duration
	InterpolateParams bool
	UnitName          string
}

var pools = struct {
	sync.RWMutex
	values map[string]*sql.DB
}{values: make(map[string]*sql.DB)}

type dbInitializer func(map[string]*DBConf) error
type dbGetter func(string) *sql.DB

type operationError struct {
	operation string
	cause     error
}

func (e operationError) Error() string {
	return "mysql: " + e.operation + " database failed"
}

func (e operationError) Unwrap() error {
	return e.cause
}

func DBInit(config Config) error {
	return dbInit(config, defaultDBInit)
}

func dbInit(config Config, initializer dbInitializer) error {
	if err := validate(config); err != nil {
		return err
	}

	settings := map[string]*DBConf{
		config.Name: newDBConf(config),
	}

	if err := initializer(settings); err != nil {
		return operationError{operation: "initialize", cause: err}
	}
	return nil
}

func GetDB(name string) *sql.DB {
	return getDB(name, defaultGetDB)
}

func getDB(name string, getter dbGetter) *sql.DB {
	return getter(name)
}

func validate(config Config) error {
	if strings.TrimSpace(config.Name) == "" {
		return errors.New("mysql: Name is required")
	}
	if strings.TrimSpace(config.DBName) == "" {
		return errors.New("mysql: DBName is required")
	}
	if strings.TrimSpace(config.Host) == "" {
		return errors.New("mysql: Host is required")
	}
	if strings.TrimSpace(config.User) == "" {
		return errors.New("mysql: User is required")
	}
	if config.Port < 1 || config.Port > 65535 {
		return errors.New("mysql: Port must be between 1 and 65535")
	}
	if config.ConnectTimeout < 0 {
		return errors.New("mysql: ConnectTimeout must not be negative")
	}
	if config.ReadTimeout < 0 {
		return errors.New("mysql: ReadTimeout must not be negative")
	}
	if config.WriteTimeout < 0 {
		return errors.New("mysql: WriteTimeout must not be negative")
	}
	if config.MaxOpenConns < 0 {
		return errors.New("mysql: MaxOpenConns must not be negative")
	}
	if config.MaxIdleConns < 0 {
		return errors.New("mysql: MaxIdleConns must not be negative")
	}
	if config.ConnMaxLifetime < 0 {
		return errors.New("mysql: ConnMaxLifetime must not be negative")
	}
	if config.ConnMaxIdleTime < 0 {
		return errors.New("mysql: ConnMaxIdleTime must not be negative")
	}
	return nil
}

func newDBConf(config Config) *DBConf {
	charset := config.Charset
	if charset == "" {
		charset = "utf8mb4"
	}
	return &DBConf{
		Dbname:            config.DBName,
		User:              config.User,
		Password:          config.Password,
		Host:              config.Host,
		Port:              config.Port,
		ReadTimeout:       config.ReadTimeout,
		WriteTimeout:      config.WriteTimeout,
		ConnectTimeout:    config.ConnectTimeout,
		Charset:           charset,
		MaxOpenConns:      config.MaxOpenConns,
		MaxIdleConns:      config.MaxIdleConns,
		ConnMaxLifeTime:   config.ConnMaxLifetime,
		ConnMaxIdleTime:   config.ConnMaxIdleTime,
		InterpolateParams: config.InterpolateParams,
		UnitName:          config.UnitName,
	}
}

func defaultDBInit(settings map[string]*DBConf) error {
	opened := make(map[string]*sql.DB, len(settings))
	for name, config := range settings {
		if config == nil {
			return errors.New("mysql: database config is nil")
		}
		pool, err := openMySQL(config)
		if err != nil {
			return err
		}
		opened[name] = pool
	}
	pools.Lock()
	pools.values = opened
	pools.Unlock()
	return nil
}

func openMySQL(config *DBConf) (*sql.DB, error) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return nil, err
	}
	driverConfig := gomysql.Config{
		User:                 config.User,
		Passwd:               config.Password,
		Net:                  "tcp",
		Addr:                 config.Host + ":" + strconv.Itoa(config.Port),
		DBName:               config.Dbname,
		ParseTime:            true,
		Loc:                  location,
		Timeout:              config.ConnectTimeout,
		ReadTimeout:          config.ReadTimeout,
		WriteTimeout:         config.WriteTimeout,
		InterpolateParams:    config.InterpolateParams,
		AllowNativePasswords: true,
	}
	if config.Charset != "" {
		driverConfig.Params = map[string]string{"charset": config.Charset}
	}
	pool, err := sql.Open("mysql", driverConfig.FormatDSN())
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(config.MaxOpenConns)
	pool.SetMaxIdleConns(config.MaxIdleConns)
	pool.SetConnMaxLifetime(config.ConnMaxLifeTime)
	pool.SetConnMaxIdleTime(config.ConnMaxIdleTime)
	if err := pool.Ping(); err != nil {
		_ = pool.Close()
		return nil, err
	}
	return pool, nil
}

func defaultGetDB(name string) *sql.DB {
	pools.RLock()
	defer pools.RUnlock()
	return pools.values[name]
}
