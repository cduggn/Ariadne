#!/usr/bin/env bash
# Remove everything setup.sh created. Dry run unless APPLY=1.
set -euo pipefail
: "${REGION:?set REGION}"
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
BUCKET="${BUCKET:-cluster-doctor-lab-${ACCOUNT}-${REGION}}"
run() { if [ "${APPLY:-0}" = 1 ]; then "$@"; else printf 'DRY-RUN:'; printf ' %q' "$@"; echo; fi; }
run aws s3 rm "s3://$BUCKET" --recursive
run aws s3api delete-bucket --bucket "$BUCKET" --region "$REGION"
for u in cluster-doctor-readonly cluster-doctor-writer; do
  for k in $(aws iam list-access-keys --user-name "$u" --query 'AccessKeyMetadata[].AccessKeyId' --output text 2>/dev/null); do
    run aws iam delete-access-key --user-name "$u" --access-key-id "$k"; done
  for p in $(aws iam list-user-policies --user-name "$u" --query 'PolicyNames[]' --output text 2>/dev/null); do
    run aws iam delete-user-policy --user-name "$u" --policy-name "$p"; done
  run aws iam delete-user --user-name "$u"
done
