"""What the pinned chat sent the bot since the last run: texts printed,
files saved to the directory in argv[1]. make tg-fetch runs it.

The token comes in TG_TOKEN and never goes to output: errors print the
method and the HTTP code, not the URL. Telegram keeps updates for 24 hours
only, and getUpdates with an offset drops everything before it for good,
so the offset is saved only after every file of the batch is on disk.
"""
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime

TOKEN = os.environ["TG_TOKEN"]
CHAT = int(os.environ["TG_CHAT"])
OUT = sys.argv[1]
OFFSET = os.path.join(OUT, ".offset")
# The Bot API serves files up to this size.
MAX_FILE = 20 * 1024 * 1024


def call(method, **params):
    url = f"https://api.telegram.org/bot{TOKEN}/{method}?" + urllib.parse.urlencode(params)
    try:
        with urllib.request.urlopen(url, timeout=30) as r:
            return json.load(r)["result"]
    except urllib.error.HTTPError as e:
        sys.exit(f"telegram {method}: {e.code}")
    except urllib.error.URLError as e:
        sys.exit(f"telegram {method}: {e.reason}")


def download(file_id, name):
    path = call("getFile", file_id=file_id)["file_path"]
    url = f"https://api.telegram.org/file/bot{TOKEN}/{path}"
    try:
        with urllib.request.urlopen(url, timeout=120) as r, open(name, "wb") as f:
            f.write(r.read())
    except urllib.error.HTTPError as e:
        sys.exit(f"telegram file: {e.code}")
    except urllib.error.URLError as e:
        sys.exit(f"telegram file: {e.reason}")


def main():
    os.makedirs(OUT, exist_ok=True)
    try:
        offset = int(open(OFFSET).read())
    except (OSError, ValueError):
        offset = 0
    updates = call("getUpdates", offset=offset, timeout=0, allowed_updates='["message"]')
    if not updates:
        print("nothing new")
        return
    for u in updates:
        m = u.get("message")
        # Anyone can write to the bot; only the pinned chat counts.
        if not m or m["chat"]["id"] != CHAT:
            continue
        when = datetime.fromtimestamp(m["date"]).strftime("%Y-%m-%d %H:%M:%S")
        text = m.get("text") or m.get("caption")
        if text:
            print(f"{when} text: {text}")
        if "document" in m:
            doc = m["document"]
            file_id, name, size = doc["file_id"], doc.get("file_name") or "file", doc.get("file_size", 0)
        elif "photo" in m:
            photo = m["photo"][-1]
            file_id, name, size = photo["file_id"], "photo.jpg", photo.get("file_size", 0)
        else:
            continue
        if size > MAX_FILE:
            print(f"{when} file: {name}, {size // 1024} KB, over the Bot API's 20 MB, not saved")
            continue
        dest = os.path.join(OUT, f"{m['message_id']}-{os.path.basename(name)}")
        download(file_id, dest)
        print(f"{when} file: {dest} ({os.path.getsize(dest) // 1024} KB)")
    with open(OFFSET, "w") as f:
        f.write(str(updates[-1]["update_id"] + 1))


main()
