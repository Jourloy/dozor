#!/usr/bin/env python3
"""Read-only sampling of a running Dozor. Does not certify recording coverage."""
import argparse
import csv
import datetime
import getpass
import http.cookiejar
import json
import os
from pathlib import Path
import sys
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description="Собрать CSV состояния Dozor при длительном испытании")
    parser.add_argument("url", help="Например, http://raspberrypi.local:8080")
    parser.add_argument("--hours", type=float, default=72)
    parser.add_argument("--interval", type=float, default=30, help="Период опроса в секундах")
    parser.add_argument("--cameras", type=int, default=4, help="Ожидаемое число включённых камер")
    parser.add_argument("--out", default="dozor-soak.csv")
    args = parser.parse_args()
    if args.hours <= 0 or args.interval < 1 or args.cameras < 1:
        parser.error("hours > 0, interval >= 1, cameras >= 1")
    base = args.url.rstrip("/")
    if not base.startswith(("http://", "https://")):
        parser.error("Нужен HTTP(S) URL")
    password = os.environ.get("DOZOR_PASSWORD") or getpass.getpass("Пароль администратора: ")
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def request(path, payload=None):
        data = None if payload is None else json.dumps(payload).encode()
        req = urllib.request.Request(base + "/api/v1" + path, data=data,
                                     headers={"Content-Type": "application/json"})
        with opener.open(req, timeout=30) as response:
            return json.load(response)

    def login():
        request("/login", {"password": password})

    login()
    columns = ["utc", "disk_ready", "online", "enabled", "detectors_healthy", "queue",
               "disk_used", "disk_total", "new_losses", "error"]
    notices = set()
    samples = failures = 0
    deadline = time.monotonic() + args.hours * 3600
    path = Path(args.out)
    # Refuse to overwrite an earlier run.
    with path.open("x", newline="", encoding="utf-8") as output:
        writer = csv.DictWriter(output, fieldnames=columns)
        writer.writeheader()
        try:
            while time.monotonic() < deadline:
                row = dict.fromkeys(columns, "")
                row["utc"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
                try:
                    try:
                        state = request("/status")
                    except urllib.error.HTTPError as error:
                        if error.code != 401:
                            raise
                        login()
                        state = request("/status")
                    cameras = [c for c in state["cameras"] if c["enabled"]]
                    losses = []
                    for notice in state["notices"]:
                        identity = (notice["at"], notice["message"])
                        if identity not in notices and "невыгруженная" in notice["message"]:
                            losses.append(notice["message"])
                        notices.add(identity)
                    row.update(disk_ready=state["disk_ready"], enabled=len(cameras),
                               online=sum(c["online"] for c in cameras),
                               detectors_healthy=sum(c["detector_healthy"] for c in cameras),
                               queue=state["queue"], disk_used=state.get("disk_used", 0),
                               disk_total=state.get("disk_total", 0), new_losses=len(losses))
                    if (not row["disk_ready"] or row["online"] != args.cameras
                            or row["detectors_healthy"] != args.cameras or losses):
                        failures += 1
                except (OSError, ValueError, KeyError):
                    row["error"] = "status unavailable or invalid"
                    failures += 1
                writer.writerow(row)
                output.flush()
                samples += 1
                time.sleep(min(args.interval, max(0, deadline-time.monotonic())))
        except KeyboardInterrupt:
            print("Сбор остановлен; частичный CSV сохранён.")
    print(f"{path}: {samples} замеров; {failures} замеров требуют разбора.")
    print("Проверьте видео, интервалы событий, S3 и журнал сбоев по docs/acceptance.md.")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
