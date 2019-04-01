package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"regexp"
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

	_ "github.com/go-sql-driver/mysql"
)

// These get autofilled by goreleaser
var (
	version = "dev"
	commit  = "none"
)

var myExp = regexp.MustCompile(`(?m)arn:aws:lambda:ap-southeast-1:(?P<account>\d+):function:(?P<fn>\w+)`)

type CreateProcedure struct {
	Procedure           string         `db:"Procedure"`
	SqlMode             string         `db:"sql_mode"`
	Source              sql.NullString `db:"Create Procedure"`
	CharacterSetClient  string         `db:"character_set_client"`
	CollationConnection string         `db:"collation_connection"`
	DatabaseCollation   string         `db:"Database Collation"`
}

type Procedures struct {
	Db                  string    `db:"Db"`
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

	h.DSN = fmt.Sprintf("%s:%s@tcp(%s:3306)/bugzilla?parseTime=true&multiStatements=true&sql_mode=TRADITIONAL&collation=utf8mb4_unicode_ci",
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
	prometheus.MustRegister(h.userGroupMapCount())
	prometheus.MustRegister(h.slowLogEnabled())
	prometheus.MustRegister(h.iamEnabled())
	prometheus.MustRegister(h.insync())

	addr := ":" + os.Getenv("PORT")
	app := h.BasicEngine()

	if err := http.ListenAndServe(addr, app); err != nil {
		log.WithError(err).Fatal("error listening")
	}

}

func (h handler) unicode(w http.ResponseWriter, r *http.Request) {

	type config struct {
		Key   string `db:"Variable_name"`
		Value string `db:"Value"`
		Good  bool
	}
	var configuration []config
	err := h.db.Select(&configuration, `show variables where variable_name like 'character\_set\_%' or variable_name like 'collation%';`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for i := 0; i < len(configuration); i++ {
		if configuration[i].Value == "utf8mb4" || configuration[i].Value == "utf8mb4_unicode_ci" {
			configuration[i].Good = true
		}
		if configuration[i].Key == "character_set_system" && configuration[i].Value == "utf8" {
			configuration[i].Good = true
		}
		if configuration[i].Key == "character_set_filesystem" && configuration[i].Value == "binary" {
			configuration[i].Good = true
		}

	}

	var t = template.Must(template.New("").Parse(`<ol>
{{- range . }}
{{- if .Good }}
<li>{{ .Key }} - {{ .Value }}</li>
{{ else }}
<li style="color: red">{{ .Key }} - {{ .Value }}</li>
{{- end }}
{{- end }}
</ol>`))

	w.Header().Set("Content-Type", "text/html")
	t.Execute(w, configuration)
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
			resp, err := req.Send()
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
	var output string
	for i, v := range pp {
		if !strings.HasPrefix(v.Name, "lambda") {
			continue
		}

		if v.Name == "lambda_async" {
			continue
		}

		var src CreateProcedure
		// There must be an easier way
		log.Infof("Switching to: %s", v.Db)
		h.db.MustExec(fmt.Sprintf("use %s", v.Db))
		err := h.db.QueryRow(fmt.Sprintf("SHOW CREATE PROCEDURE %s", v.Name)).Scan(&src.Procedure, &src.SqlMode, &src.Source, &src.CharacterSetClient, &src.CollationConnection, &src.DatabaseCollation)
		if err != nil {
			log.WithError(err).WithField("name", v.Name).Error("failed to get procedure source")
			output += fmt.Sprintf("<h1 style='color: orange;'>%d: No source found for %s</h1>\n", i, v.Name)
			continue
		}

		result := findNamedMatches(myExp, src.Source.String)
		// log.Infof("account: %s fn: %s\n", result["account"], result["fn"])

		// log.WithField("name", v.Name).Infof("src: %#v", &src.Source)
		output += fmt.Sprintf("<h1>%d: %s</h1>\n", i, v.Name)
		if result["fn"] == "alambda_simple" {
			if result["account"] != h.AccountID {
				output += fmt.Sprintf("<h2 style='color: red;'>Account ID %s != %s</h2>\n", result["account"], h.AccountID)
			}
		} else {
			output += fmt.Sprintf("<h2 style='color: yellow;'>Function %s != %s</h2>\n", result["fn"], "alambda_simple")
		}
		output += fmt.Sprintf("<pre>%s</pre>\n", src.Source.String)
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, output)
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
	hzs, err := req.Send()
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
	listrecords, err := req.Send()
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

func findNamedMatches(regex *regexp.Regexp, str string) map[string]string {
	match := regex.FindStringSubmatch(str)

	results := map[string]string{}
	for i, name := range match {
		results[regex.SubexpNames()[i]] = name
	}
	return results
}
