#!/usr/bin/env python3
"""Boot a produced CLI and read its embedded checkout HTML and JavaScript.

Uses a fresh owned database on OPENRAILS_SMOKE_DSN and the configured test Redis.
No merchant, customer, PSP or payment is created. Nothing is read from source dist.
"""
import argparse
import hashlib
from html.parser import HTMLParser
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


class PageAssets(HTMLParser):
    def __init__(self):
        super().__init__()
        self.root = False
        self.scripts = []

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        self.root |= tag == "div" and attrs.get("id") == "root"
        if tag == "script" and attrs.get("src"):
            self.scripts.append(attrs["src"])


def free_port():
    with socket.socket() as connection:
        connection.bind(("127.0.0.1", 0))
        return connection.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--target", default="linux_amd64")
    args = parser.parse_args()
    if not args.target.startswith("linux_amd64"):
        return  # The same embedded assets are checked on the CI-native target.
    binary = args.binary.resolve(strict=True)
    report = args.report.resolve()
    report.parent.mkdir(parents=True, exist_ok=True)
    dsn = os.environ["OPENRAILS_SMOKE_DSN"]
    redis = os.environ["OPENRAILS_SMOKE_REDIS_ADDR"]
    database = "openrails_native_assets_" + uuid.uuid4().hex
    parsed = urllib.parse.urlsplit(dsn)
    owned_dsn = urllib.parse.urlunsplit(parsed._replace(path="/" + database))
    log_path = report.with_suffix(".server.log")

    def sql(query):
        subprocess.run(["psql", dsn, "-X", "-v", "ON_ERROR_STOP=1", "-q", "-c", query],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)

    sql("CREATE DATABASE " + database)
    child = None
    try:
        with tempfile.TemporaryDirectory(prefix="openrails-native-assets-") as temporary:
            temporary = Path(temporary)
            api_port, checkout_port = free_port(), free_port()
            while checkout_port == api_port:
                checkout_port = free_port()
            origin = f"http://127.0.0.1:{api_port}"
            checkout = f"127.0.0.1:{checkout_port}"
            config = {
                "host": "127.0.0.1", "port": api_port,
                "db": {"url": owned_dsn}, "redis": {"addr": redis},
                "provider_write_mode": "readonly", "public_billing_base_url": origin,
                "checkout_origin": "http://" + checkout, "route_groups": {"admin": True},
                "local_sign_in": True,
                "auth": {
                    "default_tenant_id": str(uuid.uuid4()),
                    "token": {"issuer": origin}, "resource": {"id": origin},
                    "keys": {"path": str(temporary / "keys.json"), "allow_ephemeral_dev_keys": True},
                    "http": {"direct_peer_ip": True},
                    "registration": {"native_user_mode": "closed", "allow_missing_senders": True},
                },
            }
            config_path = temporary / "config.json"
            config_path.write_text(json.dumps(config))
            config_path.chmod(0o600)
            # Do not inherit another application's DB/auth/provider configuration.
            env = {key: os.environ[key] for key in ("PATH", "HOME", "LANG", "TZ") if key in os.environ}
            with log_path.open("wb") as log:
                child = subprocess.Popen([str(binary), "--config", str(config_path), "run-server"],
                                         cwd=temporary, env=env, stdout=log, stderr=subprocess.STDOUT)

                def get(path, host=None):
                    request = urllib.request.Request(origin + path, headers={"Host": host} if host else {})
                    with urllib.request.urlopen(request, timeout=3) as response:
                        return response.headers.get_content_type(), response.read()

                deadline = time.monotonic() + 90
                while True:
                    if child.poll() is not None:
                        raise RuntimeError("native server exited; see " + str(log_path))
                    try:
                        get("/health/ready")
                        break
                    except (urllib.error.URLError, TimeoutError, OSError):
                        if time.monotonic() >= deadline:
                            raise RuntimeError("native server readiness timed out; see " + str(log_path))
                        time.sleep(0.2)
                content_type, html = get("/pay/ord_00000000-0000-4000-8000-000000000001", checkout)
                assert content_type == "text/html", "checkout returned no HTML"
                page = PageAssets()
                page.feed(html.decode("utf-8"))
                assert page.root, "checkout application root is absent"
                scripts = []
                for src in page.scripts:
                    url = urllib.parse.urlsplit(src)
                    if url.netloc and url.netloc != checkout:
                        continue  # Vendor-hosted resources are not this artifact's assets.
                    assert url.path.startswith("/assets/"), "unexpected embedded checkout script path"
                    mime, script = get(url.path + ("?" + url.query if url.query else ""), checkout)
                    assert "javascript" in mime and len(script) > 100, "checkout script was not served"
                    scripts.append({"path": url.path, "sha256": hashlib.sha256(script).hexdigest(), "bytes": len(script)})
                assert scripts, "checkout HTML references no embedded JavaScript"
                report.write_text(json.dumps({"binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                                              "checkout_html_sha256": hashlib.sha256(html).hexdigest(),
                                              "scripts": scripts, "provider_write_mode": "readonly"}, indent=2) + "\n")
                print("Native checkout HTML and", len(scripts), "embedded JavaScript asset(s) passed")
    finally:
        if child is not None and child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=15)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait(timeout=5)
        sql("DROP DATABASE " + database)


if __name__ == "__main__":
    main()
