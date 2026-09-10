#!/usr/bin/env bash
set -euo pipefail

# Sets up api.makwatches.in as a custom domain for the HTTP API created by
# deploy.sh. Steps 1-3 create AWS-side resources only and do NOT affect
# production traffic. Step 4's DNS record change (in Cloudflare, where this
# domain's DNS actually lives -- this AWS account has no Route53 zone for it)
# is what actually cuts traffic over, and is called out explicitly below: do
# it yourself, when ready, not via this script.

AWS_REGION="${AWS_REGION:-ap-south-1}"
DOMAIN="${DOMAIN:-api.makwatches.in}"
HTTP_API_NAME="${HTTP_API_NAME:-makwatches-http-api}"

API_ID=$(aws apigatewayv2 get-apis --region "$AWS_REGION" --query "Items[?Name=='$HTTP_API_NAME'].ApiId" --output text)
[ -n "$API_ID" ] && [ "$API_ID" != "None" ] || { echo "HTTP API $HTTP_API_NAME not found -- run deploy.sh first" >&2; exit 1; }

CERT_ARN=$(aws acm list-certificates --region "$AWS_REGION" --query "CertificateSummaryList[?DomainName=='$DOMAIN'].CertificateArn" --output text)
if [ -z "$CERT_ARN" ] || [ "$CERT_ARN" == "None" ]; then
  CERT_ARN=$(aws acm request-certificate --domain-name "$DOMAIN" --validation-method DNS --region "$AWS_REGION" --query CertificateArn --output text)
  echo "==> Requested cert $CERT_ARN"
  sleep 5
fi

echo ""
echo "===> ACTION REQUIRED (Cloudflare dashboard, not this CLI):"
aws acm describe-certificate --certificate-arn "$CERT_ARN" --region "$AWS_REGION" \
  --query 'Certificate.DomainValidationOptions[0].ResourceRecord' --output table
echo "Add the Name/Value above as a CNAME record in Cloudflare (proxy: DNS only/grey cloud)."
echo "Then re-run this script, or wait here:"
read -p "Press Enter once the CNAME is added in Cloudflare... " _

aws acm wait certificate-validated --certificate-arn "$CERT_ARN" --region "$AWS_REGION"
echo "==> Certificate validated"

if ! aws apigatewayv2 get-domain-name --domain-name "$DOMAIN" --region "$AWS_REGION" >/dev/null 2>&1; then
  aws apigatewayv2 create-domain-name --domain-name "$DOMAIN" \
    --domain-name-configurations "CertificateArn=$CERT_ARN,EndpointType=REGIONAL,SecurityPolicy=TLS_1_2" \
    --region "$AWS_REGION" >/dev/null
  echo "==> Created custom domain $DOMAIN"
fi

if ! aws apigatewayv2 get-api-mappings --domain-name "$DOMAIN" --region "$AWS_REGION" --query "Items[?ApiId=='$API_ID']" --output text | grep -q .; then
  aws apigatewayv2 create-api-mapping --domain-name "$DOMAIN" --api-id "$API_ID" --stage '$default' --region "$AWS_REGION" >/dev/null
  echo "==> Mapped $DOMAIN -> $HTTP_API_NAME \$default stage"
fi

TARGET=$(aws apigatewayv2 get-domain-name --domain-name "$DOMAIN" --region "$AWS_REGION" \
  --query 'DomainNameConfigurations[0].ApiGatewayDomainName' --output text)

echo ""
echo "===================== FINAL STEP (production cutover -- do this yourself, when ready) ====================="
echo "In Cloudflare DNS for makwatches.in, change the CNAME for 'api' from:"
echo "    1budcsbl.up.railway.app"
echo "to:"
echo "    $TARGET"
echo "Keep proxy status 'DNS only' (grey cloud)."
echo "This is the one step in the whole migration that actually moves production traffic. Nothing else does."
echo "=============================================================================================================="
