#!/usr/bin/env python3
"""Allow weekday nights (22:00–08:00) and all weekend, Asia/Shanghai."""
import argparse
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo


def remaining_seconds(now):
    if now.tzinfo is None:
        raise ValueError("timezone-aware time required")
    local = now.astimezone(ZoneInfo("Asia/Shanghai"))
    weekday = local.weekday()
    if weekday < 5 and 8 <= local.hour < 22:
        return 0
    if weekday >= 5:
        end_day = local.date() + timedelta(days=7 - weekday)
    elif weekday == 4 and local.hour >= 22:
        end_day = local.date() + timedelta(days=3)
    else:
        end_day = local.date() + timedelta(days=1 if local.hour >= 22 else 0)
    end = datetime.combine(end_day, datetime.min.time(), local.tzinfo) + timedelta(hours=8)
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
