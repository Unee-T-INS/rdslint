We want to capture across our uneet-{dev,demo,prod} accounts:

- [ ] uptime
- [X] engineversion
- [X] status
- [X] dbInstanceClass
- [X] user_group_map count - so we know if there has been any truncation issues
- [X] schema_version - so we know what version of the data structure we are running
- [X] aurora_version - so we know what version of the database we are running
- [ ] snapshot_time (PreferredBackupWindow) - so we know at what time snapshots are being taken
- [ ] BackupRetentionPeriod - so we know how far back we can restore
- [X] insync - so we know if all our settings are in affect
- [ ] binlog_time - whether binlogs are enabled and how far they go
- [X] iam_auth - whether IAM auth is enabled
- [X] slow_log - whether slow log is enabled, with log_output & log_queries_not_using_indexes
- [ ] general_log - whether general log is enabled
- [X] cluster_endpoint - so we know what the cluster endpoint URL is
- [ ] backtrack - if we can back track and what is the window
- [ ] cloudwatch - check whether logs are being sent to CloudWatch
- [X] check [lambda_async](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/AuroraMySQL.Integrating.Lambda.html) is present
- [ ] check triggers are enabled
- [X] check innodb_file_format

https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_LogAccess.Concepts.MySQL.html

# Policy notes

Requires:

* AmazonRoute53ReadOnlyAccess
* AmazonRDSReadOnlyAccess

# TODO

Perhaps not sent heartbeat for lambda_async and [check
permissions](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/AuroraMySQL.Integrating.Lambda.html#AuroraMySQL.Integrating.NativeLambda)
instead, i.e. check the role has been applied.

A role's policy looks like:

	{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Sid": "VisualEditor1",
				"Effect": "Allow",
				"Action": "lambda:*",
				"Resource": "arn:aws:lambda:ap-southeast-1:812644853088:function:alambda_simple"
			}
		]
	}


Check for `lambda:*` in an attached role I guess!

Also need to ensure the user has correct permissions:

	mysql> GRANT EXECUTE ON *.* TO 'lambda_invoker'@'%';
	Query OK, 0 rows affected (0.01 sec)

Check user actually exists

## Check lambda invoker user is in place in the parameter store

	[hendry@t480s tests]$ ssm uneet-dev LAMBDA_INVOKER_USERNAME
	aws --profile uneet-dev ssm get-parameters --names LAMBDA_INVOKER_USERNAME --with-decryption --query Parameters[0].Value --output text
	lambda_invoker
	[hendry@t480s tests]$ ssm uneet-demo LAMBDA_INVOKER_USERNAME
	aws --profile uneet-demo ssm get-parameters --names LAMBDA_INVOKER_USERNAME --with-decryption --query Parameters[0].Value --output text
	None
	[hendry@t480s tests]$ ssm uneet-prod LAMBDA_INVOKER_USERNAME
	aws --profile uneet-prod ssm get-parameters --names LAMBDA_INVOKER_USERNAME --with-decryption --query Parameters[0].Value --output text
	None
