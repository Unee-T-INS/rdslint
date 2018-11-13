package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws/endpoints"
	"github.com/aws/aws-sdk-go-v2/aws/external"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/unee-t/env"

	"github.com/apex/log"
	jsonhandler "github.com/apex/log/handlers/json"

	"database/sql"

	_ "github.com/go-sql-driver/mysql"
)

// These get autofilled by goreleaser
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type handler struct {
	DSN            string // e.g. "bugzilla:secret@tcp(auroradb.dev.unee-t.com:3306)/bugzilla?multiStatements=true&sql_mode=TRADITIONAL"
	APIAccessToken string // e.g. O8I9svDTizOfLfdVA5ri
	db             *sql.DB
}

func init() {
	log.SetHandler(jsonhandler.Default)
	if s := os.Getenv("UP_STAGE"); s != "" {
		version = s
	}
	if v := os.Getenv("UP_COMMIT"); v != "" {
		commit = v
	}

}

// New setups the configuration assuming various parameters have been setup in the AWS account
func New() (h handler, err error) {

	cfg, err := external.LoadDefaultAWSConfig(external.WithSharedConfigProfile("uneet-dev"))
	if err != nil {
		log.WithError(err).Fatal("setting up credentials")
		return
	}
	cfg.Region = endpoints.ApSoutheast1RegionID
	e, err := env.New(cfg)
	if err != nil {
		log.WithError(err).Warn("error getting unee-t env")
	}

	// Check for MYSQL_HOST override
	var mysqlhost string
	val, ok := os.LookupEnv("MYSQL_HOST")
	if ok {
		log.Infof("MYSQL_HOST overridden by local env: %s", val)
		mysqlhost = val
	} else {
		mysqlhost = e.Udomain("auroradb")
	}

	h = handler{
		DSN: fmt.Sprintf("%s:%s@tcp(%s:3306)/bugzilla?multiStatements=true&sql_mode=TRADITIONAL",
			e.GetSecret("MYSQL_USER"),
			e.GetSecret("MYSQL_PASSWORD"),
			mysqlhost),
		APIAccessToken: e.GetSecret("API_ACCESS_TOKEN"),
	}

	h.db, err = sql.Open("mysql", h.DSN)
	if err != nil {
		log.WithError(err).Fatal("error opening database")
		return
	}

	return

}

func (h handler) BasicEngine() http.Handler {
	app := mux.NewRouter()
	app.HandleFunc("/", h.ping).Methods("GET")
	app.Handle("/metrics", promhttp.Handler()).Methods("GET")
	return app
}

func main() {

	h, err := New()
	if err != nil {
		log.WithError(err).Fatal("error setting configuration")
		return
	}

	defer h.db.Close()

	dbinfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dbinfo",
			Help: "A metric with a constant '1' value labeled by the Unee-T schema version, Aurora version and lambda commit.",
		},
		[]string{"schemaversion", "aversion", "commit"},
	)

	schemaversion := h.schemaversion()
	aversion := h.aversion()
	dbinfo.WithLabelValues(schemaversion, aversion, commit).Set(1)

	// TODO: Implement a collector
	// i.e. I am using the "direct instrumentation" approach atm
	// https://github.com/prometheus/docs/blob/master/content/docs/instrumenting/writing_exporters.md#collectors
	prometheus.MustRegister(dbinfo)
	prometheus.MustRegister(h.userGroupMapCount())

	addr := ":" + os.Getenv("PORT")
	app := h.BasicEngine()

	if err := http.ListenAndServe(addr, env.Protect(app, h.APIAccessToken)); err != nil {
		log.WithError(err).Fatal("error listening")
	}

}

func (h handler) ping(w http.ResponseWriter, r *http.Request) {
	ctx := log.WithFields(log.Fields{
		"reqid": r.Header.Get("X-Request-Id"),
		"UA":    r.Header.Get("User-Agent"),
	})
	err := h.db.Ping()
	if err != nil {
		ctx.WithError(err).Error("failed to ping database")
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	fmt.Fprintf(w, "OK")
}

func (h handler) schemaversion() (version string) {

	rows, err := h.db.Query("SET @highest_id = (SELECT MAX(`id`) FROM `ut_db_schema_version`); SELECT `schema_version` FROM `ut_db_schema_version` WHERE `id` = @highest_id;")
	if err != nil {
		log.WithError(err).Error("failed to open database")
		return
	}
	defer rows.Close()

	for rows.Next() {
		if err := rows.Scan(&version); err != nil {
			log.WithError(err).Error("failed to scan version")
		}
	}
	return version

}

func (h handler) aversion() (aversion string) {

	rows, err := h.db.Query("select AURORA_VERSION()")
	if err != nil {
		log.WithError(err).Error("failed to open database")
		return
	}
	defer rows.Close()

	for rows.Next() {
		if err := rows.Scan(&aversion); err != nil {
			log.WithError(err).Error("failed to scan version")
		}
	}
	return aversion

}

func (h handler) userGroupMapCount() (countMetric prometheus.Gauge) {
	rows, err := h.db.Query("select COUNT(*) from user_group_map")
	if err != nil {
		log.WithError(err).Error("failed to open database")
		return
	}
	defer rows.Close()

	// Would be nice to just be an int
	var count float64

	for rows.Next() {
		if err := rows.Scan(&count); err != nil {
			log.WithError(err).Error("failed to scan count")
		}
	}
	log.Infof("Count: %d", count)

	countMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "user_group_map_total", Help: "shows the number of rows in the user_group_map table."})

	countMetric.Set(count)

	return countMetric
}
