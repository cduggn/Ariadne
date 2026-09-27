#!/usr/bin/env bash
# Create the tiny AWS footprint for the runaway-cost scenario (D-27). NOT run automatically.
#   AWS_PROFILE=<profile> REGION=eu-west-1 bash deploy/aws/setup.sh            # dry run: prints the commands
#   AWS_PROFILE=<profile> REGION=eu-west-1 APPLY=1 bash deploy/aws/setup.sh    # really creates
# Creates: one private bucket (objects expire after 1 day), a read-only IAM user for the doctor and a
# put-only IAM user for the writer pod. Access keys are printed ONCE to stdout — store them in a
# Kubernetes Secret or your shell, never in git. Expected cost per scenario run: well under $1.
set -euo pipefail
: "${REGION:?set REGION}"
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
BUCKET="${BUCKET:-cluster-doctor-lab-${ACCOUNT}-${REGION}}"
run() { if [ "${APPLY:-0}" = 1 ]; then "$@"; else printf 'DRY-RUN:'; printf ' %q' "$@"; echo; fi; }
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
sed "s/__BUCKET__/$BUCKET/g" "$here/policies/doctor-readonly.json" > "$tmp/ro.json"
sed "s/__BUCKET__/$BUCKET/g" "$here/policies/writer.json" > "$tmp/w.json"

if [ "$REGION" = us-east-1 ]; then run aws s3api create-bucket --bucket "$BUCKET" --region "$REGION"
else run aws s3api create-bucket --bucket "$BUCKET" --region "$REGION" --create-bucket-configuration LocationConstraint="$REGION"; fi
run aws s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
  BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
run aws s3api put-bucket-lifecycle-configuration --bucket "$BUCKET" --lifecycle-configuration "file://$here/lifecycle.json"
run aws s3api put-bucket-tagging --bucket "$BUCKET" --tagging 'TagSet=[{Key=project,Value=cluster-doctor}]'

run aws iam create-user --user-name cluster-doctor-readonly --tags Key=project,Value=cluster-doctor
run aws iam put-user-policy --user-name cluster-doctor-readonly --policy-name doctor-readonly --policy-document "file://$tmp/ro.json"
run aws iam create-user --user-name cluster-doctor-writer --tags Key=project,Value=cluster-doctor
run aws iam put-user-policy --user-name cluster-doctor-writer --policy-name lab-writer --policy-document "file://$tmp/w.json"
run aws iam create-access-key --user-name cluster-doctor-readonly
run aws iam create-access-key --user-name cluster-doctor-writer
echo "bucket: $BUCKET   (export DOCTOR_S3_BUCKET=$BUCKET for the doctor's live backend)"
