#!/usr/bin/env python3
"""Fail closed outside weekday 22:00–next-day 08:00, Asia/Shanghai."""
import argparse
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo


def remaining_seconds(now):
    if now.tzinfo is None:
        raise ValueError("timezone-aware time required")
    local = now.astimezone(ZoneInfo("Asia/Shanghai"))
    start_day = local.date() if local.hour >= 22 else local.date() - timedelta(days=1)
    if start_day.weekday() >= 5 or 8 <= local.hour < 22:
        return 0
    end = datetime.combine(start_day + timedelta(days=1), datetime.min.time(), local.tzinfo) + timedelta(hours=8)
    return max(0, (end - local).total_seconds())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--minimum-remaining-minutes", type=int, default=0)
    args = parser.parse_args()
    if args.minimum_remaining_minutes < 0:
        parser.error("remaining time must not be negative")
    seconds = remaining_seconds(datetime.now(timezone.utc))
    if seconds <= 0 or seconds < args.minimum_remaining_minutes * 60:
        raise SystemExit("Public delivery window closed or insufficient time remains (Asia/Shanghai).")
    print("Public delivery window open (Asia/Shanghai).")


if __name__ == "__main__":
    main()
