package sql

import (
	"fmt"

	"github.com/pkg/errors"

	"github.com/cs3org/reva/v3/cmd/revad/pkg/config"
	ocmshareregistry "github.com/cs3org/reva/v3/pkg/ocm/share/repository/registry"
	publicshareregistry "github.com/cs3org/reva/v3/pkg/publicshare/manager/registry"
	shareregistry "github.com/cs3org/reva/v3/pkg/share/manager/registry"
	model "github.com/cs3org/reva/v3/pkg/share/manager/sql/model"
	"github.com/cs3org/reva/v3/pkg/sharedconf"
	"github.com/cs3org/reva/v3/pkg/utils/dbutil"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const (
	projectSpaceGroupsPrefix      = "cernbox-project-"
	projectSpaceAdminGroupsSuffix = "-admins"
	projectPathPrefix             = "/eos/project/"
)

type Config struct {
	config.Database      `mapstructure:",squash"`
	GatewaySvc           string `mapstructure:"gatewaysvc"`
	LinkPasswordHashCost int    `mapstructure:"password_hash_cost"`
}

func init() {
	shareregistry.Register("sql", NewShareManager)
	publicshareregistry.Register("sql", NewPublicShareManager)
	ocmshareregistry.Register("sql", NewOCMShareManager)
}

func (c *Config) ApplyDefaults() {
	c.GatewaySvc = sharedconf.GetGatewaySVC(c.GatewaySvc)
	c.Database = sharedconf.GetDBInfo(c.Database)
	// PasswordHashCost is the number of bcrypt rounds,
	// which should be at least 11
	if c.LinkPasswordHashCost < 11 {
		c.LinkPasswordHashCost = 11
	}
}

func getDb(c Config) (*gorm.DB, error) {
	gormCfg := &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: false,
	}
	var (
		db  *gorm.DB
		err error
	)
	switch c.Engine {
	case "sqlite":
		db, err = gorm.Open(sqlite.Open(c.DBName), gormCfg)
	case "postgres":
		dsn := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", c.DBHost, c.DBPort, c.DBUsername, c.DBPassword, c.DBName)
		db, err = gorm.Open(postgres.Open(dsn), gormCfg)
	case "mysql":
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true", c.DBUsername, c.DBPassword, c.DBHost, c.DBPort, c.DBName)
		db, err = gorm.Open(mysql.Open(dsn), gormCfg)
	default: // default is mysql
		dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true", c.DBUsername, c.DBPassword, c.DBHost, c.DBPort, c.DBName)
		db, err = gorm.Open(mysql.Open(dsn), gormCfg)
	}
	if err != nil {
		return nil, err
	}
	if err := dbutil.ConfigurePool(db); err != nil {
		return nil, errors.Wrap(err, "failed to configure DB connection pool")
	}
	return db, nil
}

func createID(db *gorm.DB) (uint, error) {
	id := &model.ShareID{}

	res := db.Create(&id)
	if res.Error != nil {
		return 0, res.Error
	} else {
		return id.ID, nil
	}
}
