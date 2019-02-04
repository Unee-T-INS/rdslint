package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/endpoints"
	"github.com/aws/aws-sdk-go-v2/aws/external"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tj/go/http/response"
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

type dbinfo struct {
	Cluster rds.DBCluster
	DBs     []rds.DBInstance
	Params  []rds.Parameter
}

type handler struct {
	AWSCfg         aws.Config
	DSN            string // e.g. "bugzilla:secret@tcp(auroradb.dev.unee-t.com:3306)/bugzilla?multiStatements=true&sql_mode=TRADITIONAL"
	APIAccessToken string
	mysqlhost      string
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

	h = handler{
		AWSCfg:         cfg,
		mysqlhost:      e.Udomain("auroradb"),
		APIAccessToken: e.GetSecret("API_ACCESS_TOKEN"),
	}

	h.DSN = fmt.Sprintf("%s:%s@tcp(%s:3306)/bugzilla?multiStatements=true&sql_mode=TRADITIONAL",
		e.GetSecret("MYSQL_USER"),
		e.GetSecret("MYSQL_PASSWORD"),
		h.mysqlhost)

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
	app.HandleFunc("/describe", h.describe).Methods("GET")
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

	dbcheck := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "dbinfo",
			Help: "A metric with a constant '1' value labeled by the Unee-T schema version, Aurora version and lambda commit.",
		},
		[]string{"schemaversion", "auroraversion", "commit"},
	)

	schemaversion := h.schemaversion()
	aversion := h.aversion()
	dbcheck.WithLabelValues(schemaversion, aversion, commit).Set(1)

	// TODO: Implement a collector
	// i.e. I am using the "direct instrumentation" approach atm
	// https://github.com/prometheus/docs/blob/master/content/docs/instrumenting/writing_exporters.md#collectors
	prometheus.MustRegister(dbcheck)
	prometheus.MustRegister(h.userGroupMapCount())

	addr := ":" + os.Getenv("PORT")
	app := h.BasicEngine()

	if err := http.ListenAndServe(addr, app); err != nil {
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
		return
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
	log.Infof("Parsed version %s", version)
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
	log.Infof("Count: %f", count)

	countMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "user_group_map_total", Help: "shows the number of rows in the user_group_map table."})

	countMetric.Set(count)

	return countMetric
}

func (h handler) lookupHostedZone() (string, error) {
	// https://godoc.org/github.com/aws/aws-sdk-go-v2/service/route53#example-Route53-GetHostedZoneRequest-Shared00
	r53 := route53.New(h.AWSCfg)
	req := r53.ListHostedZonesRequest(&route53.ListHostedZonesInput{})
	hzs, err := req.Send()
	if err != nil {
		return "", err
	}
	for _, v := range hzs.HostedZones {
		name := strings.TrimRight(*v.Name, ".")
		if h.mysqlhost[len(h.mysqlhost)-len(name):] == name {
			return *v.Id, err
		}
	}
	return "", fmt.Errorf("no hosted zone found for %s", h.mysqlhost)
}

func (h handler) lookupClusterName() (string, error) {
	r53 := route53.New(h.AWSCfg)
	hz, err := h.lookupHostedZone()
	if err != nil {
		return "", err
	}
	req := r53.ListResourceRecordSetsRequest(&route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(hz),
	})
	listrecords, err := req.Send()
	for _, v := range listrecords.ResourceRecordSets {
		// log.Infof("Name: %s", *v.Name)
		if *v.Name == h.mysqlhost+"." {
			return strings.TrimRight(*v.AliasTarget.DNSName, "."), err
		}
	}
	return "", fmt.Errorf("no alias found for %s", h.mysqlhost)
}

func (h handler) describeCluster() (dbInfo dbinfo, err error) {

	dnsEndpoint, err := h.lookupClusterName()
	if err != nil {
		return dbInfo, err
	}

	rdsapi := rds.New(h.AWSCfg)
	req := rdsapi.DescribeDBClustersRequest(&rds.DescribeDBClustersInput{})
	result, err := req.Send()
	if err != nil {
		return dbInfo, err
	}
	for _, v := range result.DBClusters {
		if *v.Endpoint == dnsEndpoint {
			dbInfo.Cluster = v
			// https://godoc.org/github.com/aws/aws-sdk-go-v2/service/rds#example-RDS-DescribeDBInstancesRequest-Shared00
			for _, db := range v.DBClusterMembers {
				req := rdsapi.DescribeDBInstancesRequest(&rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(*db.DBInstanceIdentifier)})
				result, err := req.Send()
				if err != nil {
					return dbInfo, err
				}
				dbInfo.DBs = append(dbInfo.DBs, result.DBInstances...)
				// log.Infof("Result: %#v", result.DBInstances)

			}
			for _, db := range dbInfo.DBs {
				groupName := db.DBParameterGroups[0].DBParameterGroupName

				for _, group := range db.DBParameterGroups {
					if groupName != group.DBParameterGroupName {
						log.Errorf("Differing parameter groups! %q != %q", *groupName, *group.DBParameterGroupName)
					}
					req := rdsapi.DescribeDBParametersRequest(&rds.DescribeDBParametersInput{
						DBParameterGroupName: aws.String(*group.DBParameterGroupName),
					})

					p := req.Paginate()
					for p.Next() {
						page := p.CurrentPage()
						dbInfo.Params = append(dbInfo.Params, page.Parameters...)
						// log.Infof("Page: %#v", page)
					}

				}
			}

			return dbInfo, err
		}
	}
	return dbInfo, fmt.Errorf("no cluster info found for %s", h.mysqlhost)
}

func (h handler) describe(w http.ResponseWriter, r *http.Request) {
	rdsCluster, err := h.describeCluster()
	if err != nil {
		log.WithError(err).Error("failed to find database info")
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	response.JSON(w, rdsCluster)
}
