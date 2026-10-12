#!/usr/bin/env python3
"""Prune old image builds from this repository's GHCR container package.

Every push publishes a multi-arch image: one tagged index version plus one
untagged manifest version per platform. The per-platform versions look like
garbage in the package list but are what the index points at, so they must
never be deleted on their own. This script therefore works on whole builds:

  * a build is a *tagged* version (the index); builds are ordered by age;
  * the newest --keep builds are kept together with every manifest their index
    references;
  * every other version is deleted (old indexes and their platform manifests,
    plus orphans), except versions younger than --min-age-minutes.

It refuses to delete anything when it cannot read an index it needs to keep.

Environment: GH_TOKEN (needs packages: write), GITHUB_REPOSITORY.
"""
import argparse
import base64
import json
import os
import subprocess
import sys
import urllib.request
from datetime import datetime, timedelta, timezone

INDEX_ACCEPT = ", ".join([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
])


def tags_of(version):
    return (version.get("metadata", {}).get("container", {}) or {}).get("tags") or []


def parse_time(s):
    return datetime.strptime(s, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)


def plan(versions, children_of, keep, now, min_age):
    """Return (kept_builds, to_delete). Pure: no network, no side effects.

    children_of(digest) -> iterable of child manifest digests (may raise).
    """
    if keep < 1:
        raise ValueError("--keep must be at least 1")
    builds = sorted((v for v in versions if tags_of(v)),
                    key=lambda v: v["created_at"], reverse=True)
    if not builds:
        raise RuntimeError("no tagged version found; refusing to delete anything")
    kept = builds[:keep]
    keep_digests = set()
    for v in kept:
        keep_digests.add(v["name"])
        keep_digests.update(children_of(v["name"]))
    cutoff = now - min_age
    doomed = [v for v in versions
              if v["name"] not in keep_digests and parse_time(v["created_at"]) <= cutoff]
    return kept, doomed


def gh_api(path, method="GET"):
    out = subprocess.run(["gh", "api", "-X", method, path],
                         check=True, capture_output=True, text=True).stdout
    return json.loads(out) if out.strip() else None


def list_versions(base):
    versions, page = [], 1
    while True:
        batch = gh_api(f"{base}/versions?per_page=100&page={page}")
        if not batch:
            return versions
        versions.extend(batch)
        if len(batch) < 100:
            return versions
        page += 1


def registry_token(owner, package, token):
    basic = base64.b64encode(f"{owner}:{token}".encode()).decode()
    req = urllib.request.Request(
        f"https://ghcr.io/token?service=ghcr.io&scope=repository:{owner}/{package}:pull",
        headers={"Authorization": f"Basic {basic}"})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["token"]


def make_children_of(owner, package, token):
    bearer = registry_token(owner, package, token)

    def children_of(digest):
        req = urllib.request.Request(
            f"https://ghcr.io/v2/{owner}/{package}/manifests/{digest}",
            headers={"Authorization": f"Bearer {bearer}", "Accept": INDEX_ACCEPT})
        with urllib.request.urlopen(req, timeout=30) as r:
            doc = json.load(r)
        return [m["digest"] for m in doc.get("manifests", [])]

    return children_of


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--keep", type=int, default=5, help="image builds to keep (default 5)")
    ap.add_argument("--min-age-minutes", type=int, default=30,
                    help="never delete versions newer than this (default 30)")
    ap.add_argument("--dry-run", action="store_true", help="print the plan, delete nothing")
    ap.add_argument("--package", help="package name (default: repository name)")
    args = ap.parse_args()

    repo = os.environ["GITHUB_REPOSITORY"]
    owner, name = repo.split("/", 1)
    package = args.package or name
    token = os.environ.get("GH_TOKEN") or os.environ.get("GITHUB_TOKEN")
    if not token:
        sys.exit("GH_TOKEN is required")

    owner_type = gh_api(f"users/{owner}")["type"]
    base = (f"orgs/{owner}" if owner_type == "Organization" else f"users/{owner}") \
        + f"/packages/container/{package}"

    versions = list_versions(base)
    kept, doomed = plan(versions, make_children_of(owner, package, token),
                        args.keep, datetime.now(timezone.utc),
                        timedelta(minutes=args.min_age_minutes))

    print(f"package {owner}/{package}: {len(versions)} versions, "
          f"keeping {len(kept)} build(s), deleting {len(doomed)}")
    for v in kept:
        print(f"  keep   {v['id']}  {v['name'][:19]}  tags={','.join(tags_of(v))}")
    for v in sorted(doomed, key=lambda v: v["created_at"], reverse=True):
        print(f"  delete {v['id']}  {v['name'][:19]}  tags={','.join(tags_of(v)) or '-'}")
        if not args.dry_run:
            gh_api(f"{base}/versions/{v['id']}", method="DELETE")
    if args.dry_run:
        print("dry run: nothing deleted")


if __name__ == "__main__":
    main()
