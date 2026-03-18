#!/usr/bin/env python3
import argparse
import glob
import os
import sys
import uuid
from io import BytesIO
from urllib.request import Request, urlopen
from urllib.error import HTTPError, URLError


def build_multipart(field_name, filename, data_bytes, boundary):
    buf = BytesIO()

    def writeln(s):
        if isinstance(s, str):
            s = s.encode("utf-8")
        buf.write(s)
        buf.write(b"\r\n")

    writeln(f"--{boundary}")
    writeln(f"Content-Disposition: form-data; name=\"{field_name}\"; filename=\"{filename}\"")
    writeln("Content-Type: application/json")
    writeln("")
    buf.write(data_bytes)
    buf.write(b"\r\n")
    writeln(f"--{boundary}--")
    return buf.getvalue()


def upload_file(url, path, key, timeout):
    with open(path, "rb") as f:
        data_bytes = f.read()

    boundary = "----cpa-boundary-" + uuid.uuid4().hex
    body = build_multipart("file", os.path.basename(path), data_bytes, boundary)

    headers = {
        "Content-Type": f"multipart/form-data; boundary={boundary}",
    }
    if key:
        headers["Authorization"] = f"Bearer {key}"

    req = Request(url, data=body, headers=headers, method="POST")
    with urlopen(req, timeout=timeout) as resp:
        return resp.status, resp.read().decode("utf-8", errors="replace")


def main():
    parser = argparse.ArgumentParser(description="Upload all JSON auth files in current directory.")
    parser.add_argument("--url", default="http://localhost:8317/v0/management/auth-files", help="Management upload URL")
    parser.add_argument("--key", default=os.getenv("MANAGEMENT_KEY") or os.getenv("CPA_MANAGEMENT_KEY") or "", help="Management key (Bearer)")
    parser.add_argument("--glob", dest="pattern", default="*.json", help="Glob pattern for JSON files")
    parser.add_argument("--timeout", type=int, default=30, help="HTTP timeout in seconds")
    args = parser.parse_args()

    files = sorted([p for p in glob.glob(args.pattern) if os.path.isfile(p)])
    if not files:
        print(f"No files matched pattern: {args.pattern}")
        return 1

    print(f"Uploading {len(files)} file(s) to {args.url}")

    failed = 0
    for path in files:
        try:
            status, body = upload_file(args.url, path, args.key, args.timeout)
            if status != 200:
                failed += 1
                print(f"[FAIL] {path} -> HTTP {status} :: {body[:200]}")
            else:
                print(f"[OK]   {path}")
        except HTTPError as e:
            failed += 1
            detail = e.read().decode("utf-8", errors="replace") if e.fp else ""
            print(f"[FAIL] {path} -> HTTP {e.code} :: {detail[:200]}")
        except URLError as e:
            failed += 1
            print(f"[FAIL] {path} -> {e}")
        except Exception as e:
            failed += 1
            print(f"[FAIL] {path} -> {e}")

    if failed:
        print(f"Done with {failed} failure(s).")
        return 2

    print("Done.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
