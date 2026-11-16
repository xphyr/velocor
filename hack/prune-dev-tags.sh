#!/usr/bin/env bash
# Remove outdated dev-<sha> builds from the registry.
#
# A dev build is the set of images tagged dev-<sha> (velocor-spdk, -velod, -csi, -os); builds
# are ranked by commit order from git and kept or removed as a whole. Only dev-* tags are ever
# considered: latest, v* and any other tag are never touched. Deletion is by tag
# (DELETE /v2/<repo>/manifests/<tag>), never by digest, because consecutive builds often share
# a digest (an unchanged image) and deleting the digest would remove the newer tag too.
#
# Dry run unless --apply is given.
#
# Usage: hack/prune-dev-tags.sh [--keep-last N] [--keep dev-<sha> ...] [--apply]
#   --keep-last N   newest builds to keep (default 1)
#   --keep TAG      also keep this build (repeatable), e.g. one an appliance is running
#   env IMAGE_PREFIX (required)  <registry>/<org> the dev builds were pushed to
#   env REGISTRY_USER / REGISTRY_TOKEN_FILE (required for --apply)  a robot account with push
#        rights and a file holding its token
set -euo pipefail

: "${IMAGE_PREFIX:?set IMAGE_PREFIX to <registry>/<org>}"
REGISTRY=${IMAGE_PREFIX%%/*}
ORG=${IMAGE_PREFIX#*/}
REPOS=(velocor-spdk velocor-velod velocor-csi velocor-os)
KEEP_LAST=1 APPLY=false KEEP=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep-last) KEEP_LAST="$2"; shift 2 ;;
    --keep) KEEP+=("$2"); shift 2 ;;
    --apply) APPLY=true; shift ;;
    *) sed -n 2,17p "$0"; exit 2 ;;
  esac
done
cd "$(dirname "$0")/.."
USER_NAME=${REGISTRY_USER:-}
TOKEN_FILE=${REGISTRY_TOKEN_FILE:-}

# Every dev tag in any repository.
declare -A present
for repo in "${REPOS[@]}"; do
  for tag in $(skopeo list-tags --no-creds "docker://$REGISTRY/$ORG/$repo" | python3 -c 'import json,sys; print("\n".join(json.load(sys.stdin)["Tags"]))'); do
    [[ "$tag" =~ ^dev-[0-9a-f]{7,40}$ ]] || continue
    # Quay keeps listing deleted tags while they can still be revived ("time machine");
    # only count tags that still resolve.
    skopeo inspect --no-creds --raw "docker://$REGISTRY/$ORG/$repo:$tag" >/dev/null 2>&1 || continue
    present[$tag]+="$repo "
  done
done
[[ ${#present[@]} -gt 0 ]] || { echo "no dev builds in $REGISTRY/$ORG"; exit 0; }

# Newest first, by commit time; builds whose commit isn't in this clone sort last (oldest).
ranked=$(for tag in "${!present[@]}"; do
  ts=$(git show -s --format=%ct "${tag#dev-}" 2>/dev/null || echo 0)
  echo "$ts $tag"
done | sort -rn | awk '{print $2}')

declare -A keep
n=0
for tag in $ranked; do
  if (( n < KEEP_LAST )); then keep[$tag]="newest"; fi
  n=$((n+1))
done
for tag in "${KEEP[@]}"; do keep[$tag]="--keep"; done

delete=()
for tag in $ranked; do
  if [[ -n "${keep[$tag]:-}" ]]; then
    echo "keep    $tag (${keep[$tag]}) in ${present[$tag]}"
  else
    echo "delete  $tag from ${present[$tag]}"
    delete+=("$tag")
  fi
done
[[ ${#delete[@]} -gt 0 ]] || { echo "nothing to delete"; exit 0; }
if ! $APPLY; then echo "(dry run: pass --apply to delete)"; exit 0; fi

[[ -n "$USER_NAME" && -f "$TOKEN_FILE" ]] || { echo "--apply needs REGISTRY_USER and REGISTRY_TOKEN_FILE" >&2; exit 1; }
for tag in "${delete[@]}"; do
  for repo in ${present[$tag]}; do
    token=$(curl -fsS -u "$USER_NAME:$(tr -d '\n\r' < "$TOKEN_FILE")" \
      "https://$REGISTRY/v2/auth?service=$REGISTRY&scope=repository:$ORG/$repo:pull,push" |
      python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
    code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $token" \
      "https://$REGISTRY/v2/$ORG/$repo/manifests/$tag")
    if [[ "$code" == 202 ]]; then echo "deleted $ORG/$repo:$tag"; else echo "FAILED  $ORG/$repo:$tag (HTTP $code)" >&2; fi
  done
done
