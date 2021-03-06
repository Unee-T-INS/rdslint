package main

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/aws/endpoints"
	"github.com/aws/aws-sdk-go-v2/aws/external"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tj/go/http/response"
	"github.com/unee-t/env"

	"github.com/apex/log"
	jsonhandler "github.com/apex/log/handlers/json"
	texthandler "github.com/apex/log/handlers/text"

	"github.com/go-sql-driver/mysql"
	_ "github.com/go-sql-driver/mysql"
)

// These get autofilled by goreleaser
var (
	version = "dev"
	commit  = "none"
)

var myExp = regexp.MustCompile(`(?m)arn:aws:lambda:ap-southeast-1:(?P<account>\d+):function:(?P<fn>\w+)`)

type CreateProcedure struct {
	Database            string
	Procedure           string         `db:"Procedure"`
	SqlMode             string         `db:"sql_mode"`
	Source              sql.NullString `db:"Create Procedure"`
	CharacterSetClient  string         `db:"character_set_client"`
	CollationConnection string         `db:"collation_connection"`
	DatabaseCollation   string         `db:"Database Collation"`
	AccountCheck        template.HTML
	CorrectCollation    bool
}

type TableInfo struct {
	Field   string         `db:"Field"`
	Type    string         `db:"Type"`
	Null    sql.NullString `db:"Null"`
	Key     string         `db:"Key"`
	Default sql.NullString `db:"Default"`
	Extra   string         `db:"Extra"`
}

type Procedures struct {
	Database            string    `db:"Db"`
	Name                string    `db:"Name"`
	Definer             string    `db:"Definer"`
	Type                string    `db:"Type"`
	Modified            time.Time `db:"Modified"`
	Created             time.Time `db:"Created"`
	SecurityType        string    `db:"Security_type"`
	Comment             string    `db:"Comment"`
	CharacterSetClient  string    `db:"character_set_client"`
	CollationConnection string    `db:"collation_connection"`
	DatabaseCollation   string    `db:"Database Collation"`
}

type dbinfo struct {
	Cluster rds.DBCluster
	DBs     []rds.DBInstance
	Params  []rds.Parameter
}

type handler struct {
	AWSCfg         aws.Config
	DSN            string
	APIAccessToken string
	LambdaInvoker  string
	mysqlhost      string
	AccountID      string
	db             *sqlx.DB
	dbInfo         dbinfo
}

func init() {
	log.SetHandler(texthandler.Default)
	if s := os.Getenv("UP_STAGE"); s != "" {
		log.SetHandler(jsonhandler.Default)
		version = s
	}
	if v := os.Getenv("UP_COMMIT"); v != "" {
		commit = v
	}

}

// New setups the configuration assuming various parameters have been setup in the AWS account
func New() (h handler, err error) {

	cfg, err := external.LoadDefaultAWSConfig(external.WithSharedConfigProfile("uneet-prod"))
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
		AccountID:      e.AccountID,
		LambdaInvoker:  e.GetSecret("LAMBDA_INVOKER_USERNAME"),
		mysqlhost:      e.Udomain("auroradb"),
		APIAccessToken: e.GetSecret("API_ACCESS_TOKEN"),
	}

	h.DSN = fmt.Sprintf("%s:%s@tcp(%s:3306)/bugzilla?parseTime=true&multiStatements=true&sql_mode=TRADITIONAL&collation=utf8mb4_unicode_520_ci",
		"root",
		e.GetSecret("MYSQL_ROOT_PASSWORD"),
		h.mysqlhost)

	h.db, err = sqlx.Open("mysql", h.DSN)
	if err != nil {
		log.WithError(err).Fatal("error opening database")
		return
	}
	h.dbInfo, err = h.describeCluster()
	if err != nil {
		log.WithError(err).Fatal("error collecting info")
		return
	}

	return

}

func (h handler) BasicEngine() http.Handler {
	app := mux.NewRouter()
	app.HandleFunc("/", h.ping).Methods("GET")
	app.HandleFunc("/call", h.call).Methods("GET")
	app.HandleFunc("/checks", h.checks).Methods("GET")
	app.HandleFunc("/unicode", h.unicode).Methods("GET")
	app.HandleFunc("/tables", h.tables).Methods("GET")
	app.HandleFunc("/describe", func(w http.ResponseWriter, r *http.Request) { response.JSON(w, h.dbInfo) }).Methods("GET")
	app.Handle("/metrics", promhttp.Handler()).Methods("GET")
	log.Infof("STAGE: %s", os.Getenv("UP_STAGE"))

	if os.Getenv("UP_STAGE") == "" {
		// local dev, get around permissions
		return app
	}

	return env.Protect(app, h.APIAccessToken)

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
		[]string{"schemaversion",
			"auroraversion",
			"commit",
			"engineversion",
			"instanceclass",
			"endpoint",
			"innodb_file_format",
			"status"},
	)

	dbcheck.WithLabelValues(h.schemaversion(),
		h.aversion(),
		commit,
		h.engineVersion(),
		h.instanceClass(),
		*h.dbInfo.Cluster.Endpoint,
		h.innodbFileFormat(),
		// https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/Overview.DBInstance.Status.html
		*h.dbInfo.Cluster.Status).Set(1)

	// TODO: Implement a collector
	// i.e. I am using the "direct instrumentation" approach atm
	// https://github.com/prometheus/docs/blob/master/content/docs/instrumenting/writing_exporters.md#collectors
	// but it's lambda, so can we assume it goes cold ??
	prometheus.MustRegister(dbcheck)
	// prometheus.MustRegister(h.userGroupMapCount())
	prometheus.MustRegister(h.slowLogEnabled())
	prometheus.MustRegister(h.iamEnabled())
	prometheus.MustRegister(h.insync())

	addr := ":" + os.Getenv("PORT")
	app := h.BasicEngine()

	if err := http.ListenAndServe(addr, app); err != nil {
		log.WithError(err).Fatal("error listening")
	}

}

func (h handler) tables(w http.ResponseWriter, r *http.Request) {
	var tables []string
	err := h.db.Select(&tables, `show tables`)
	if err != nil {
		log.WithError(err).Errorf("failed to show tables")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	smallint := make(map[string]int)

	for _, t := range tables {
		var tinfo []TableInfo
		err := h.db.Select(&tinfo, fmt.Sprintf("describe %s", t))
		if err != nil {
			log.WithError(err).WithField("table", t).Errorf("failed to describe table")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if strings.Contains(tinfo[0].Type, "smallint") {
			var count int
			err := h.db.Get(&count, fmt.Sprintf("select COUNT(*) from %s", t))
			if err != nil {
				log.WithError(err).WithField("table", t).Errorf("failed to count table")
			}
			smallint[t] = count
		}
	}

	// https://stackoverflow.com/a/44380276/4534

	type kv struct {
		Key   string
		Value int
	}

	var ss []kv
	for k, v := range smallint {
		ss = append(ss, kv{k, v})
	}

	sort.Slice(ss, func(i, j int) bool {
		return ss[i].Value > ss[j].Value
	})

	response.OK(w, ss)
}

func (h handler) unicode(w http.ResponseWriter, r *http.Request) {

	type tableStatus struct {
		Name          string         `db:"Name"`
		Engine        sql.NullString `db:"Engine"`
		Version       sql.NullString `db:"Version"`
		RowFormat     sql.NullString `db:"Row_format"`
		Rows          sql.NullString `db:"Rows"`
		AvgRowLength  sql.NullString `db:"Avg_row_length"`
		DataLength    sql.NullString `db:"Data_length"`
		MaxDataLength sql.NullString `db:"Max_data_length"`
		IndexLength   sql.NullString `db:"Index_length"`
		DataFree      sql.NullString `db:"Data_free"`
		AutoIncrement sql.NullInt64  `db:"Auto_increment"`
		CreateTime    mysql.NullTime `db:"Create_time"`
		UpdateTime    mysql.NullTime `db:"Update_time"`
		CheckTime     mysql.NullTime `db:"Check_time"`
		Checksum      sql.NullString `db:"Checksum"`
		CreateOptions sql.NullString `db:"Create_options"`
		Comment       sql.NullString `db:"Comment"`
		Collation     sql.NullString `db:"Collation"`
	}

	type showCreate struct {
		Database       string `db:"Database"`
		CreateDatabase string `db:"Create Database"`
	}
	type dbunicode struct {
		Name   string
		Info   []showCreate
		Tables []tableStatus
	}
	dbinfo := []dbunicode{{Name: "bugzilla"}, {Name: "unee_t_enterprise"}}

	for j := 0; j < len(dbinfo); j++ {
		h.db.MustExec(fmt.Sprintf("use %s", dbinfo[j].Name))
		err := h.db.Select(&dbinfo[j].Info, fmt.Sprintf("SHOW CREATE DATABASE %s;", dbinfo[j].Name))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		err = h.db.Select(&dbinfo[j].Tables, `SHOW TABLE STATUS;`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	var t = template.Must(template.New("").Parse(`<!DOCTYPE html>
<html lang=en>
<head>
<meta charset="utf-8">
<title>Unicode test</title>
<meta name="viewport" content="width=device-width, initial-scale=1.0"/>
<style>
body { padding: 1rem; font-family: "Open Sans", "Segoe UI", "Seravek", sans-serif; }
</style>
<body>
{{- range . }}
<h1>{{ .Name }}</h1>
{{ range .Info }}
<p>{{ .CreateDatabase }}</p>
{{ end }}
<ol>
{{- range .Tables }}
{{ if .Collation.Valid }}
<li>{{ .Name }} - 

{{ if eq .Collation.String "utf8mb4_unicode_520_ci" }}
{{ .Collation.String }}
{{ else }}
<span style="color:red">{{ .Collation.String }}</span>
{{ end }}

</li>
{{ else }}
<li>{{ .Name }} - Missing collation</li>
{{ end }}
{{- end }}
</ol>
{{- end }}
</body></html>`))
	err := t.Execute(w, dbinfo)
	if err != nil {
		log.WithError(err).Error("template failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (h handler) checks(w http.ResponseWriter, r *http.Request) {

	if h.LambdaInvoker == "" {
		http.Error(w, "LAMBDA_INVOKER_USERNAME is unset", http.StatusInternalServerError)
		return
	}

	var invokerExists bool
	err := h.db.Get(&invokerExists, `SELECT EXISTS(SELECT 1 FROM mysql.user WHERE user = ?)`, h.LambdaInvoker)
	if err != nil {
		log.WithError(err).Errorf("failed to select %s", h.LambdaInvoker)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !invokerExists {
		http.Error(w, fmt.Sprintf("LAMBDA_INVOKER_USERNAME: %s does not exist", h.LambdaInvoker), http.StatusInternalServerError)
		return
	}

	var grants []string
	err = h.db.Select(&grants, fmt.Sprintf("show grants for %s", h.LambdaInvoker))
	if err != nil {
		log.WithError(err).Errorf("failed to get grants for %s", h.LambdaInvoker)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Infof("Grants: %#v", grants)
	var executePerms bool
	for _, v := range grants {
		log.Infof("Checking: %q", v)
		if v == fmt.Sprintf("GRANT EXECUTE ON *.* TO '%s'@'%%'", h.LambdaInvoker) {
			executePerms = true
			break
		}
	}
	if !executePerms {
		http.Error(w, fmt.Sprintf("LAMBDA_INVOKER_USERNAME: %s does not have execute permissions", h.LambdaInvoker), http.StatusInternalServerError)
		return
	}

	var lambdaAccess bool
	for _, v := range h.dbInfo.Cluster.AssociatedRoles {
		log.WithField("status", v.Status).Infof("Role: %#v", v)
		if *v.Status == "ACTIVE" {
			a, err := arn.Parse(*v.RoleArn)
			if err != nil {
				log.WithError(err).Errorf("failed to get arn for %s", *v.RoleArn)
				continue
			}
			log.Infof("Checking RoleArn: %s has lambda perms", a.Resource)
			i := iam.New(h.AWSCfg)
			// https://godoc.org/github.com/aws/aws-sdk-go-v2/service/iam#IAM.ListAttachedRolePoliciesRequest
			req := i.ListAttachedRolePoliciesRequest(&iam.ListAttachedRolePoliciesInput{
				RoleName: aws.String(strings.TrimPrefix(a.Resource, "role/")),
			})
			// aws --profile uneet-prod iam list-attached-role-policies --role-name Aurora_access_to_lambda
			resp, err := req.Send(context.TODO())
			if err != nil {
				log.WithError(err).Error("failed to get policies")
				return
			}
			log.Infof("list-attached-role-policies: %#v", resp)
			for _, v := range resp.AttachedPolicies {
				log.Infof("Policy: %#v", v)
				if *v.PolicyArn == "arn:aws:iam::aws:policy/AWSLambdaFullAccess" {
					lambdaAccess = true
					break
				}
			}
		} else {
			log.Warnf("%#v not ACTIVE", v)
		}
	}

	if !lambdaAccess {
		http.Error(w, "Active Cluster.AssociatedRoles is missing the AWSLambdaFullAccess policy", http.StatusInternalServerError)
		return
	}

	pp := []Procedures{}
	err = h.db.Select(&pp, `SHOW PROCEDURE STATUS`)
	if err != nil {
		log.WithError(err).Error("failed to make SHOW PROCEDURE STATUS listing")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// log.Infof("Results: %#v", pp)
	var procsInfo []CreateProcedure
	for _, v := range pp {
		if v.Database == "sys" {
			continue
		}
		if v.Database == "mysql" {
			continue
		}

		var src CreateProcedure
		// There must be an easier way
		log.Debugf("Switching to: %s", v.Database)
		h.db.MustExec(fmt.Sprintf("use %s", v.Database))
		src.Database = v.Database
		err := h.db.QueryRow(fmt.Sprintf("SHOW CREATE PROCEDURE %s", v.Name)).Scan(&src.Procedure, &src.SqlMode, &src.Source, &src.CharacterSetClient, &src.CollationConnection, &src.DatabaseCollation)
		if err != nil {
			log.WithError(err).WithField("name", v.Name).Error("failed to get procedure source")
			continue
		}

		if strings.HasPrefix(v.Name, "lambda") {
			result := findNamedMatches(myExp, src.Source.String)
			// log.Infof("account: %s fn: %s\n", result["account"], result["fn"])
			// log.WithField("name", v.Name).Infof("src: %#v", &src.Source)
			output := fmt.Sprintf("Fn: %s Account: %s", result["fn"], result["account"])
			if result["fn"] == "alambda_simple" {
				if result["account"] != h.AccountID {
					output += fmt.Sprintf("<span style='color: red;'>Account ID %s != %s</span>\n", result["account"], h.AccountID)
				}
			} else {
				output += fmt.Sprintf("<span style='color: red;'>Function %s != %s</span>\n", result["fn"], "alambda_simple")
			}
			src.AccountCheck = template.HTML(output)
		}

		if src.DatabaseCollation == "utf8mb4_unicode_520_ci" && src.CharacterSetClient == "utf8mb4" {
			src.CorrectCollation = true
		}

		procsInfo = append(procsInfo, src)

	}

	rejig := map[string][]CreateProcedure{}
	for _, v := range procsInfo {
		rejig[v.Database] = append(rejig[v.Database], v)
	}

	// log.Infof("%#v", procsInfo)
	var t = template.Must(template.New("").Funcs(template.FuncMap{
		"IncorrectCount": func(procs []CreateProcedure) (wrong int) {
			for _, v := range procs {
				if !v.CorrectCollation {
					wrong++
				}
			}
			return
		},
	}).Parse(`<html>
<head>
<meta charset="utf-8">
<title>Database checks</title>
<meta name="viewport" content="width=device-width, initial-scale=1.0"/>
<style>
body { padding: 1rem; font-family: "Open Sans", "Segoe UI", "Seravek", sans-serif; }
pre {
  max-width: 10em;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
pre:hover {
  max-width: none;
  white-space: pre;
}
</style>
</head>
<body>

{{ range $key, $value := . }}
<h2>Database: {{ $key }}</h2>
<p>Issues: {{ IncorrectCount . }} / {{ len . }}</p>

<ol>
{{- range . }}
<li>
<h4>Procedure: {{ .Procedure }}</h4>

{{- if eq .DatabaseCollation "utf8mb4_unicode_520_ci" }}
<span>DatabaseCollation: {{ .DatabaseCollation }}</span>
{{ else }}
<span style="color: red">DatabaseCollation: {{ .DatabaseCollation }}</span>
{{ end }}

{{- if eq .CharacterSetClient "utf8mb4"  }}
<span>CharacterSetClient: {{ .CharacterSetClient }}</span>
{{ else }}
<span style="color: red">CharacterSetClient: {{ .CharacterSetClient }}</span>
{{ end }}

{{ if .AccountCheck }}
<p>Lambda ARN check: {{ .AccountCheck }}</p>
{{ end }}
</li>

{{- end }}
</ol>
{{ end }}
</body>
</html>`))
	err = t.Execute(w, rejig)
	if err != nil {
		log.WithError(err).Error("template")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (h handler) call(w http.ResponseWriter, r *http.Request) {
	_, err := h.db.Query(fmt.Sprintf(`CALL mysql.lambda_async( 'arn:aws:lambda:ap-southeast-1:%s:function:alambda_simple', '{ "heartbeat": "%s"}' )`,
		h.AccountID, time.Now()))
	if err != nil {
		log.WithError(err).Error("failed to make mysql.lambda_async call")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "OK")
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

func (h handler) innodbFileFormat() (format string) {
	err := h.db.Get(&format, "SELECT @@innodb_file_format")
	if err != nil {
		log.WithError(err).Error("failed to get innodb_file_format version")
		return
	}
	return format
}

func (h handler) schemaversion() (version string) {
	err := h.db.Get(&version, "SET @highest_id = (SELECT MAX(`id`) FROM `ut_db_schema_version`); SELECT `schema_version` FROM `ut_db_schema_version` WHERE `id` = @highest_id;")
	if err != nil {
		log.WithError(err).Error("failed to get unee-t version")
		return
	}
	return version
}

func (h handler) aversion() (aversion string) {
	err := h.db.Get(&aversion, "select AURORA_VERSION()")
	if err != nil {
		log.WithError(err).Error("failed to get AWS Aurora version")
		return
	}
	return aversion
}

func (h handler) userGroupMapCount() (countMetric prometheus.Gauge) {
	var count float64
	err := h.db.Get(&count, "select COUNT(*) from user_group_map")
	if err != nil {
		log.WithError(err).Error("failed to get count")
		return
	}
	log.Infof("Count: %f", count)
	countMetric = prometheus.NewGauge(prometheus.GaugeOpts{Name: "user_group_map_total", Help: "shows the number of rows in the user_group_map table."})
	countMetric.Set(count)
	return countMetric
}

func (h handler) instanceClass() string {
	for _, db := range h.dbInfo.DBs {
		if *db.DBInstanceClass != "" {
			return *db.DBInstanceClass
		}
	}
	return ""
}

func (h handler) engineVersion() string {
	for _, db := range h.dbInfo.DBs {
		if *db.EngineVersion != "" {
			return *db.EngineVersion
		}
	}
	return ""
}

func (h handler) insync() (countMetric prometheus.Gauge) {
	countMetric = prometheus.NewGauge(prometheus.GaugeOpts{Name: "insync", Help: "shows whether we are in-sync with the parameter groups"})
	for _, db := range h.dbInfo.Cluster.DBClusterMembers {
		if *db.DBClusterParameterGroupStatus != "in-sync" {
			log.WithFields(log.Fields{
				"db": db.DBInstanceIdentifier,
			}).Warn("not in-sync")
			return countMetric
		}
	}

	for _, db := range h.dbInfo.DBs {
		for _, groups := range db.DBParameterGroups {
			if *groups.ParameterApplyStatus != "in-sync" {
				log.WithFields(log.Fields{
					"db":         db.DBInstanceIdentifier,
					"paramgroup": groups.DBParameterGroupName,
				}).Warn("not in-sync")
				return countMetric
			}
		}
	}
	countMetric.Set(1)
	return countMetric
}

func (h handler) iamEnabled() (countMetric prometheus.Gauge) {
	countMetric = prometheus.NewGauge(prometheus.GaugeOpts{Name: "iam", Help: "shows whether IAM auth is enabled or not."})
	for _, db := range h.dbInfo.DBs {
		if *db.IAMDatabaseAuthenticationEnabled {
			log.WithField("endpoint", db.Endpoint.Address).Info("IAM ENABLED")
			countMetric.Set(1)
		} else {
			log.WithField("endpoint", db.Endpoint.Address).Warn("IAM NOT enabled")
		}
	}
	return countMetric
}

func (h handler) slowLogEnabled() *prometheus.GaugeVec {
	slowcheck := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "slowlog",
			Help: "A metric with a constant '1' value labeled with slow log lint.",
		},
		[]string{
			"enabled",
			"log_output",
			"log_queries_not_using_indexes"},
	)

	slowcheck.WithLabelValues(
		h.lookup("slow_query_log"),
		h.lookup("log_output"),
		h.lookup("log_queries_not_using_indexes"),
	).Set(1)

	return slowcheck
}

func (h handler) lookup(key string) string {
	for _, v := range h.dbInfo.Params {
		if *v.ParameterName == key {
			log.Infof("Looking up key: %s", key)
			if v.ParameterValue != nil {
				return *v.ParameterValue
			}
		}
	}
	return ""
}

func (h handler) lookupHostedZone() (string, error) {
	// https://godoc.org/github.com/aws/aws-sdk-go-v2/service/route53#example-Route53-GetHostedZoneRequest-Shared00
	r53 := route53.New(h.AWSCfg)
	req := r53.ListHostedZonesRequest(&route53.ListHostedZonesInput{})
	hzs, err := req.Send(context.TODO())
	if err != nil {
		return "", err
	}

	for _, v := range hzs.HostedZones {
		name := strings.TrimRight(*v.Name, ".")
		log.WithFields(log.Fields{
			"name":      name,
			"mysqlhost": h.mysqlhost,
		}).Info("looking up")

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
	listrecords, err := req.Send(context.TODO())
	for _, v := range listrecords.ResourceRecordSets {
		log.Infof("Name: %s", *v.Name)
		if *v.Name == h.mysqlhost+"." {
			log.Infof("DEBUG: %#v", v)
			return *v.ResourceRecords[0].Value, err
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
	result, err := req.Send(context.TODO())
	if err != nil {
		return dbInfo, err
	}
	for _, v := range result.DBClusters {
		if *v.Endpoint == dnsEndpoint {
			dbInfo.Cluster = v
			// https://godoc.org/github.com/aws/aws-sdk-go-v2/service/rds#example-RDS-DescribeDBInstancesRequest-Shared00

			req := rdsapi.DescribeDBClusterParametersRequest(&rds.DescribeDBClusterParametersInput{DBClusterParameterGroupName: aws.String(*v.DBClusterParameterGroup),
				Source: aws.String("user"),
			})
			result, err := req.Send(context.TODO())
			if err != nil {
				return dbInfo, err
			}
			log.WithField("DBClusterParameterGroup", *v.DBClusterParameterGroup).Info("recording cluster")

			dbInfo.Params = append(dbInfo.Params, result.Parameters...)
			log.Infof("cluster: %#v", dbInfo.Params)

			log.WithField("number of dbs", len(v.DBClusterMembers)).Info("describing instances")
			for _, db := range v.DBClusterMembers {
				req := rdsapi.DescribeDBInstancesRequest(&rds.DescribeDBInstancesInput{DBInstanceIdentifier: aws.String(*db.DBInstanceIdentifier)})
				result, err := req.Send(context.TODO())
				if err != nil {
					return dbInfo, err
				}
				dbInfo.DBs = append(dbInfo.DBs, result.DBInstances...)

			}
			for _, db := range dbInfo.DBs {
				groupName := db.DBParameterGroups[0].DBParameterGroupName

				for _, group := range db.DBParameterGroups {
					if groupName != group.DBParameterGroupName {
						log.Errorf("Differing parameter groups! %q != %q", *groupName, *group.DBParameterGroupName)
					}
					log.WithField("groupname", *group.DBParameterGroupName).Info("describing")
					req := rdsapi.DescribeDBParametersRequest(&rds.DescribeDBParametersInput{
						DBParameterGroupName: aws.String(*group.DBParameterGroupName),
						Source:               aws.String("user"),
					})

					p := rds.NewDescribeDBParametersPaginator(req)
					for p.Next(context.TODO()) {
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

func findNamedMatches(regex *regexp.Regexp, str string) map[string]string {
	match := regex.FindStringSubmatch(str)

	results := map[string]string{}
	for i, name := range match {
		results[regex.SubexpNames()[i]] = name
	}
	return results
}
