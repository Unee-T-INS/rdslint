DEVUPJSON = '.profile |= "uneet-dev" \
		  |.stages.production |= (.domain = "dbcheck.dev.unee-t.com" | .zone = "dev.unee-t.com") \
		  | .actions[0].emails |= ["kai.hendry+dbcheckdev@unee-t.com"] \
		  | .lambda.vpc.subnets |= [ "subnet-0e123bd457c082cff", "subnet-0ff046ccc4e3b6281", "subnet-0e123bd457c082cff" ] \
		  | .lambda.vpc.security_groups |= [ "sg-0b83472a34bc17400", "sg-0f4dadb564041855b" ]'

DEMOUPJSON = '.profile |= "uneet-demo" \
		  |.stages.production |= (.domain = "dbcheck.demo.unee-t.com" | .zone = "demo.unee-t.com") \
		  | .actions[0].emails |= ["kai.hendry+dbcheckdemo@unee-t.com"] \
		  | .lambda.vpc.subnets |= [ "subnet-0bdef9ce0d0e2f596", "subnet-091e5c7d98cd80c0d", "subnet-0fbf1eb8af1ca56e3" ] \
		  | .lambda.vpc.security_groups |= [ "sg-6f66d316" ]'

PRODUPJSON = '.profile |= "uneet-prod" \
		  |.stages.production |= (.domain = "dbcheck.unee-t.com" | .zone = "unee-t.com") \
		  | .actions[0].emails |= ["kai.hendry+dbcheckprod@unee-t.com"] \
		  | .lambda.vpc.subnets |= [ "subnet-0df289b6d96447a84", "subnet-0e41c71ad02ee7e99", "subnet-01cb9ee064743ac56" ] \
		  | .lambda.vpc.security_groups |= [ "sg-9f5b5ef8" ]'

dev:
	@echo $$AWS_ACCESS_KEY_ID
	jq $(DEVUPJSON) up.json.in > up.json
	up deploy production

demo:
	@echo $$AWS_ACCESS_KEY_ID
	jq $(DEMOUPJSON) up.json.in > up.json
	up deploy production

prod:
	@echo $$AWS_ACCESS_KEY_ID
	jq $(PRODUPJSON) up.json.in > up.json
	up deploy production

testdev:
	curl -H "Authorization: Bearer $(shell aws --profile uneet-dev ssm get-parameters --names API_ACCESS_TOKEN --with-decryption --query Parameters[0].Value --output text)" https://dbcheck.dev.unee-t.com/version
