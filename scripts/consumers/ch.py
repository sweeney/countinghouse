"""Minimal countinghouse client, as an outside consumer would write it."""
import json, urllib.request, urllib.parse, collections

BASE = "http://127.0.0.1:8787"
CALLS = collections.Counter()

class APIError(Exception):
    def __init__(self, status, body):
        self.status, self.body = status, body
        super().__init__(f"HTTP {status}: {body}")

def get(path, **params):
    url = BASE + path
    if params:
        url += "?" + urllib.parse.urlencode(params)
    CALLS[path] += 1
    req = urllib.request.Request(url)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=60) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        raise APIError(e.code, e.read().decode()) from None

def try_get(path, **params):
    """Returns (ok, payload_or_error)."""
    try:
        return True, get(path, **params)
    except APIError as e:
        return False, e

def report(title):
    print("=" * 78)
    print(title)
    print("=" * 78)
