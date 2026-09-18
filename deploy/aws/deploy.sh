#!/usr/bin/env bash
set -euo pipefail

# Idempotent create-or-update deploy of the makwatches Lambda + API Gateway +
# SQS stack, driven entirely by the AWS CLI. Re-runnable: every resource is
# checked before being created, and code/config are always updated on
# existing functions. Custom domain / DNS cutover is a SEPARATE, explicitly
# gated script -- see deploy/aws/setup-custom-domain.sh -- never run here.
#
# Parameterized so this is reusable, unchanged, against a different AWS
# account later (e.g. the eventual "makwatches" account): nothing here
# hardcodes an account ID -- it's always resolved live via sts.

AWS_REGION="${AWS_REGION:-ap-south-1}"
PROJECT_NAME="${PROJECT_NAME:-makwatches}"
API_FUNCTION_NAME="${API_FUNCTION_NAME:-${PROJECT_NAME}-api}"
WORKER_FUNCTION_NAME="${WORKER_FUNCTION_NAME:-${PROJECT_NAME}-shipment-worker}"
API_ROLE_NAME="${API_ROLE_NAME:-${PROJECT_NAME}-api-role}"
WORKER_ROLE_NAME="${WORKER_ROLE_NAME:-${PROJECT_NAME}-worker-role}"
QUEUE_NAME="${QUEUE_NAME:-${PROJECT_NAME}-delhivery-shipments}"
DLQ_NAME="${DLQ_NAME:-${QUEUE_NAME}-dlq}"
SECRET_NAME="${SECRET_NAME:-${PROJECT_NAME}/firebase-credentials-json}"
HTTP_API_NAME="${HTTP_API_NAME:-${PROJECT_NAME}-http-api}"

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="${ENV_FILE:-$REPO_ROOT/.env}"
BUILD_DIR="$SCRIPT_DIR/build"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT   # secrets never linger on disk past this run

[ -f "$ENV_FILE" ] || { echo "ENV_FILE not found: $ENV_FILE" >&2; exit 1; }

echo "==> Building"
"$SCRIPT_DIR/build.sh"

AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
echo "==> Deploying to account $AWS_ACCOUNT_ID in $AWS_REGION as $(aws sts get-caller-identity --query Arn --output text)"

########################################
# 1. Secrets Manager: Firebase credentials
########################################
FIREBASE_JSON=$(python3 - "$ENV_FILE" <<'PY'
import sys
with open(sys.argv[1]) as f:
    for line in f:
        line = line.rstrip("\n")
        if line.startswith("FIREBASE_CREDENTIALS_JSON="):
            print(line.split("=", 1)[1])
            break
PY
)
if [ -z "$FIREBASE_JSON" ]; then
  echo "WARNING: FIREBASE_CREDENTIALS_JSON not found in $ENV_FILE -- media upload will be broken" >&2
  FIREBASE_JSON="{}"
fi
printf '%s' "$FIREBASE_JSON" > "$TMP_DIR/firebase-creds.json"

if aws secretsmanager describe-secret --secret-id "$SECRET_NAME" --region "$AWS_REGION" >/dev/null 2>&1; then
  aws secretsmanager put-secret-value --secret-id "$SECRET_NAME" \
    --secret-string "file://$TMP_DIR/firebase-creds.json" --region "$AWS_REGION" >/dev/null
  echo "==> Updated secret $SECRET_NAME"
else
  aws secretsmanager create-secret --name "$SECRET_NAME" \
    --secret-string "file://$TMP_DIR/firebase-creds.json" --region "$AWS_REGION" >/dev/null
  echo "==> Created secret $SECRET_NAME"
fi
rm -f "$TMP_DIR/firebase-creds.json"
SECRET_ARN=$(aws secretsmanager describe-secret --secret-id "$SECRET_NAME" --region "$AWS_REGION" --query ARN --output text)

########################################
# 2. SQS: DLQ, then main queue with redrive policy
########################################
ensure_queue() {
  local name="$1"
  aws sqs get-queue-url --queue-name "$name" --region "$AWS_REGION" --query QueueUrl --output text 2>/dev/null || true
}

DLQ_URL=$(ensure_queue "$DLQ_NAME")
if [ -z "$DLQ_URL" ]; then
  DLQ_URL=$(aws sqs create-queue --queue-name "$DLQ_NAME" --region "$AWS_REGION" --query QueueUrl --output text)
  echo "==> Created DLQ $DLQ_NAME"
fi
DLQ_ARN=$(aws sqs get-queue-attributes --queue-url "$DLQ_URL" --attribute-names QueueArn --region "$AWS_REGION" --query 'Attributes.QueueArn' --output text)

QUEUE_URL=$(ensure_queue "$QUEUE_NAME")
if [ -z "$QUEUE_URL" ]; then
  ATTRS=$(python3 - "$DLQ_ARN" <<'PY'
import json, sys
redrive = json.dumps({"deadLetterTargetArn": sys.argv[1], "maxReceiveCount": 3})
print(json.dumps({"VisibilityTimeout": "180", "RedrivePolicy": redrive}))
PY
)
  QUEUE_URL=$(aws sqs create-queue --queue-name "$QUEUE_NAME" --attributes "$ATTRS" --region "$AWS_REGION" --query QueueUrl --output text)
  echo "==> Created queue $QUEUE_NAME (DLQ after 3 receives, 180s visibility timeout)"
else
  echo "==> Queue $QUEUE_NAME already exists"
fi
QUEUE_ARN=$(aws sqs get-queue-attributes --queue-url "$QUEUE_URL" --attribute-names QueueArn --region "$AWS_REGION" --query 'Attributes.QueueArn' --output text)

########################################
# 3. IAM: two minimal execution roles
########################################
cat > "$TMP_DIR/trust-policy.json" <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Principal": {"Service": "lambda.amazonaws.com"}, "Action": "sts:AssumeRole"}
  ]
}
EOF

ensure_role() {
  local role_name="$1"
  if ! aws iam get-role --role-name "$role_name" >/dev/null 2>&1; then
    aws iam create-role --role-name "$role_name" --assume-role-policy-document "file://$TMP_DIR/trust-policy.json" >/dev/null
    echo "==> Created IAM role $role_name"
  fi
  aws iam attach-role-policy --role-name "$role_name" \
    --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole >/dev/null
}

ensure_role "$API_ROLE_NAME"
cat > "$TMP_DIR/api-policy.json" <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["sqs:SendMessage","sqs:SendMessageBatch"], "Resource": "$QUEUE_ARN"},
    {"Effect": "Allow", "Action": "secretsmanager:GetSecretValue", "Resource": "$SECRET_ARN"}
  ]
}
EOF
aws iam put-role-policy --role-name "$API_ROLE_NAME" --policy-name "${PROJECT_NAME}-api-inline" \
  --policy-document "file://$TMP_DIR/api-policy.json"
API_ROLE_ARN="arn:aws:iam::$AWS_ACCOUNT_ID:role/$API_ROLE_NAME"

ensure_role "$WORKER_ROLE_NAME"
cat > "$TMP_DIR/worker-policy.json" <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {"Effect": "Allow", "Action": ["sqs:ReceiveMessage","sqs:DeleteMessage","sqs:GetQueueAttributes"], "Resource": "$QUEUE_ARN"},
    {"Effect": "Allow", "Action": "secretsmanager:GetSecretValue", "Resource": "$SECRET_ARN"}
  ]
}
EOF
aws iam put-role-policy --role-name "$WORKER_ROLE_NAME" --policy-name "${PROJECT_NAME}-worker-inline" \
  --policy-document "file://$TMP_DIR/worker-policy.json"
WORKER_ROLE_ARN="arn:aws:iam::$AWS_ACCOUNT_ID:role/$WORKER_ROLE_NAME"

echo "==> Waiting 10s for IAM role propagation before first Lambda create..."
sleep 10

########################################
# 4. Env var JSON files (see render_env.py)
########################################
python3 "$SCRIPT_DIR/render_env.py" "$ENV_FILE" "$TMP_DIR/api-env.json" \
  --exclude FIREBASE_CREDENTIALS_JSON \
  --set ENVIRONMENT=production \
  --set "FRONTEND_URL=${FRONTEND_URL:-https://makwatches.in}" \
  --set "SQS_QUEUE_URL=$QUEUE_URL" \
  --set "FIREBASE_SECRET_ARN=$SECRET_ARN"

python3 "$SCRIPT_DIR/render_env.py" "$ENV_FILE" "$TMP_DIR/worker-env.json" \
  --include MONGO_URI,DATABASE_NAME,JWT_SECRET,FIREBASE_BUCKET_NAME,DELHIVERY_API_TOKEN,DELHIVERY_BASE_URL,DELHIVERY_PICKUP_LOCATION,DELHIVERY_SELLER_NAME,DELHIVERY_SELLER_PHONE,DELHIVERY_SELLER_ADDRESS,DELHIVERY_SELLER_CITY,DELHIVERY_SELLER_STATE,DELHIVERY_SELLER_PINCODE,DELHIVERY_RETURN_ADDRESS,DELHIVERY_RETURN_CITY,DELHIVERY_RETURN_STATE,DELHIVERY_RETURN_PINCODE,DELHIVERY_RETURN_PHONE \
  --set ENVIRONMENT=production \
  --set "FIREBASE_SECRET_ARN=$SECRET_ARN"

########################################
# 5. Lambda functions (create-or-update)
########################################
deploy_function() {
  local fn_name="$1" zip_path="$2" role_arn="$3" env_file="$4" timeout="$5" memory="$6"
  if aws lambda get-function --function-name "$fn_name" --region "$AWS_REGION" >/dev/null 2>&1; then
    echo "==> Updating $fn_name"
    aws lambda update-function-code --function-name "$fn_name" --zip-file "fileb://$zip_path" --region "$AWS_REGION" >/dev/null
    aws lambda wait function-updated --function-name "$fn_name" --region "$AWS_REGION"
    aws lambda update-function-configuration --function-name "$fn_name" \
      --environment "file://$env_file" --timeout "$timeout" --memory-size "$memory" --region "$AWS_REGION" >/dev/null
    aws lambda wait function-updated --function-name "$fn_name" --region "$AWS_REGION"
  else
    echo "==> Creating $fn_name"
    local tries=0
    until aws lambda create-function \
      --function-name "$fn_name" --runtime provided.al2023 --architectures arm64 \
      --handler bootstrap --role "$role_arn" --zip-file "fileb://$zip_path" \
      --timeout "$timeout" --memory-size "$memory" --environment "file://$env_file" \
      --region "$AWS_REGION" >/dev/null 2>"$TMP_DIR/create-err.log"
    do
      tries=$((tries+1))
      if grep -q "InvalidParameterValueException" "$TMP_DIR/create-err.log" && [ "$tries" -le 6 ]; then
        echo "    role not yet assumable, retrying ($tries/6)..."; sleep 5
      else
        cat "$TMP_DIR/create-err.log" >&2; return 1
      fi
    done
    aws lambda wait function-active --function-name "$fn_name" --region "$AWS_REGION"
  fi
}

# 1024 MB, raised from 512: importing a gallery decodes several 1500px
# photographs, which killed the function outright with Runtime.OutOfMemory at
# 509 MB. Memory also buys CPU on Lambda -- 512 MB is about a third of a core,
# so resizing was slow as well as tight. Roughly cost-neutral: billing is per
# GB-millisecond, and the work finishes in proportionately less time.
deploy_function "$API_FUNCTION_NAME" "$BUILD_DIR/makwatches-api.zip" "$API_ROLE_ARN" "$TMP_DIR/api-env.json" 29 1024
# 1024 MB, raised from 256: the worker now decodes and resizes imported
# photographs, and a 1500px JPEG is a 9 MB pixel buffer the moment it is
# decoded. Memory also buys CPU on Lambda.
deploy_function "$WORKER_FUNCTION_NAME" "$BUILD_DIR/makwatches-shipment-worker.zip" "$WORKER_ROLE_ARN" "$TMP_DIR/worker-env.json" 60 1024

# The API keeps a few execution slots to itself. This account's Lambda
# concurrency limit is 10 in total (the new-account default), and an import
# fans out one worker invocation per image -- so a twenty-image job took
# every slot, the panel's next poll was throttled, and API Gateway answered
# it with a bare 5xx the browser could only call a network error. Reserving
# these for the API means the worker can never starve it, whatever the
# account limit is. Raise the limit itself through Service Quotas; this
# stays correct either way.
aws lambda put-function-concurrency --function-name "$API_FUNCTION_NAME" \
  --reserved-concurrent-executions 4 --region "$AWS_REGION" >/dev/null
echo "==> Reserved 4 concurrent executions for $API_FUNCTION_NAME"

########################################
# 6. Event source mapping: queue -> worker
########################################
ESM_ID=$(aws lambda list-event-source-mappings --function-name "$WORKER_FUNCTION_NAME" \
  --event-source-arn "$QUEUE_ARN" --region "$AWS_REGION" --query "EventSourceMappings[0].UUID" --output text)
# The worker drains the queue at most WORKER_MAX_CONCURRENCY messages at a
# time. With 4 slots reserved for the API above, this keeps the two inside the
# account's limit of 10 together; a twenty-image import still finishes in a
# few waves of a couple of seconds each. Lambda's floor for this setting is 2.
WORKER_MAX_CONCURRENCY="${WORKER_MAX_CONCURRENCY:-5}"
if [ -z "$ESM_ID" ] || [ "$ESM_ID" == "None" ]; then
  aws lambda create-event-source-mapping --function-name "$WORKER_FUNCTION_NAME" \
    --event-source-arn "$QUEUE_ARN" --batch-size 1 \
    --scaling-config "MaximumConcurrency=$WORKER_MAX_CONCURRENCY" --region "$AWS_REGION" >/dev/null
  echo "==> Wired $QUEUE_NAME -> $WORKER_FUNCTION_NAME (batch size 1, max concurrency $WORKER_MAX_CONCURRENCY)"
else
  aws lambda update-event-source-mapping --uuid "$ESM_ID" \
    --scaling-config "MaximumConcurrency=$WORKER_MAX_CONCURRENCY" --region "$AWS_REGION" >/dev/null
  echo "==> Event source mapping exists; worker max concurrency set to $WORKER_MAX_CONCURRENCY"
fi

########################################
# 7. API Gateway HTTP API (default execute-api endpoint only)
########################################
API_ID=$(aws apigatewayv2 get-apis --region "$AWS_REGION" --query "Items[?Name=='$HTTP_API_NAME'].ApiId" --output text)
if [ -z "$API_ID" ] || [ "$API_ID" == "None" ]; then
  API_ID=$(aws apigatewayv2 create-api --name "$HTTP_API_NAME" --protocol-type HTTP --region "$AWS_REGION" --query ApiId --output text)
  echo "==> Created HTTP API $API_ID"
fi

API_LAMBDA_ARN="arn:aws:lambda:$AWS_REGION:$AWS_ACCOUNT_ID:function:$API_FUNCTION_NAME"
INTEGRATION_ID=$(aws apigatewayv2 get-integrations --api-id "$API_ID" --region "$AWS_REGION" \
  --query "Items[?IntegrationUri=='$API_LAMBDA_ARN'].IntegrationId" --output text)
if [ -z "$INTEGRATION_ID" ] || [ "$INTEGRATION_ID" == "None" ]; then
  INTEGRATION_ID=$(aws apigatewayv2 create-integration --api-id "$API_ID" \
    --integration-type AWS_PROXY --integration-uri "$API_LAMBDA_ARN" \
    --payload-format-version 2.0 --integration-method POST --region "$AWS_REGION" --query IntegrationId --output text)
  echo "==> Created integration $INTEGRATION_ID"
fi

ensure_route() {
  local route_key="$1"
  local existing
  existing=$(aws apigatewayv2 get-routes --api-id "$API_ID" --region "$AWS_REGION" \
    --query "Items[?RouteKey=='$route_key'].RouteId" --output text)
  if [ -z "$existing" ] || [ "$existing" == "None" ]; then
    aws apigatewayv2 create-route --api-id "$API_ID" --route-key "$route_key" \
      --target "integrations/$INTEGRATION_ID" --region "$AWS_REGION" >/dev/null
    echo "==> Created route $route_key"
  fi
}
ensure_route "ANY /{proxy+}"
ensure_route "ANY /"

if ! aws apigatewayv2 get-stage --api-id "$API_ID" --stage-name '$default' --region "$AWS_REGION" >/dev/null 2>&1; then
  aws apigatewayv2 create-stage --api-id "$API_ID" --stage-name '$default' --auto-deploy --region "$AWS_REGION" >/dev/null
  echo "==> Created \$default auto-deploy stage"
fi

aws lambda add-permission --function-name "$API_FUNCTION_NAME" \
  --statement-id "apigw-invoke-$API_ID" --action lambda:InvokeFunction \
  --principal apigateway.amazonaws.com \
  --source-arn "arn:aws:execute-api:$AWS_REGION:$AWS_ACCOUNT_ID:$API_ID/*/*" \
  --region "$AWS_REGION" >/dev/null 2>&1 || true  # already present is fine

API_ENDPOINT=$(aws apigatewayv2 get-api --api-id "$API_ID" --region "$AWS_REGION" --query ApiEndpoint --output text)

echo ""
echo "==================== DONE ===================="
echo "API endpoint:    $API_ENDPOINT"
echo "Queue URL:       $QUEUE_URL"
echo "DLQ URL:         $DLQ_URL"
echo "API function:    $API_FUNCTION_NAME"
echo "Worker function: $WORKER_FUNCTION_NAME"
echo "Test:  curl $API_ENDPOINT/health"
echo "================================================"
