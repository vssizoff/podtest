"""Smoke test against the running local dev stack (requires Python requests).

Creates uniquely named catalog entries; leaves them for inspection.
"""

from html.parser import HTMLParser
from io import BytesIO
from time import monotonic
from urllib.parse import urljoin
from uuid import uuid4
from zipfile import ZipFile

import requests


BASE = "http://localhost:8088"


class LoginForm(HTMLParser):
    action = None

    def handle_starttag(self, tag, attrs):
        fields = dict(attrs)
        if tag == "form" and fields.get("method", "").lower() == "post":
            self.action = fields.get("action")


def archive(path, content, extra=None):
    buf = BytesIO()
    with ZipFile(buf, "w") as package:
        package.writestr(path, content)
        for name, data in (extra or {}).items():
            package.writestr(name, data)
    return {"file": ("smoke.zip", buf.getvalue(), "application/zip")}


def smoke(s):
    s.trust_env = False

    def call(method, path, status=200, **kwargs):
        response = s.request(method, BASE + path, allow_redirects=False, timeout=15, **kwargs)
        assert response.status_code == status, (method, path.split("?")[0], response.status_code, response.text)
        return response

    login = call("GET", "/auth/login", 302)
    dex = login.headers["Location"]
    assert dex.startswith(BASE + "/dex/")
    local_url = dex
    for _ in range(5):
        page = s.get(local_url, allow_redirects=False, timeout=15)
        if page.status_code == 200:
            break
        assert page.status_code == 302, page.status_code
        local_url = urljoin(local_url, page.headers["Location"])
        assert local_url.startswith(BASE + "/dex/")
    else:
        raise AssertionError("Dex did not return the login form")
    form = LoginForm()
    form.feed(page.text)
    assert form.action
    action = urljoin(local_url, form.action)
    assert action.startswith(BASE + "/dex/")
    callback = call("POST", action.removeprefix(BASE), 303, data={"login": "smoke@podtest.test", "password": "podtest-local"})
    target = callback.headers["Location"]
    assert target.startswith(BASE + "/auth/callback?")
    call("GET", target.removeprefix(BASE), 303)
    session = call("GET", "/auth/session").json()
    assert session["authenticated"]
    s.headers.update({"Origin": BASE, "X-CSRF-Token": session["csrf_token"]})

    prefix = "Authoring smoke " + uuid4().hex[:8]

    def create(kind):
        return call("POST", f"/api/v1/{kind}", 201, json={"title": f"{prefix} {kind}"}).json()["id"]

    problem = create("problems")
    path = f"/api/v1/problems/{problem}"
    revision = call("POST", path + "/revisions", 201, json={"execution_format": "scripted_lua"}).json()["id"]
    draft = f"{path}/revisions/{revision}"
    call("PUT", draft + "/statements/en", json={"source": "# Smoke test"})
    call("PUT", draft + "/languages/python3", 204)
    assert "Smoke test" in call("GET", draft + "/statements").json()["items"][0]["html"]
    assert "runner.lua" in call("PUT", draft + "/package", 422, files=archive("nested/runner.lua", "return {}\n")).json()["error"]["message"]
    call("PUT", draft + "/package", 204, files=archive("runner.lua", "return {}\n"))
    assert call("POST", draft + "/validate").json()["valid"]
    call("POST", draft + "/publish", 204)
    assert call("GET", path + "/revisions").json()["items"][0]["status"] == "published"
    call("PUT", draft + "/statements/en", 409, json={"source": "Cannot edit a publication"})
    report = call("POST", "/api/v1/problems/import/polygon", files=archive("problem.xml", '<problem><names><name value="Polygon smoke"/></names></problem>')).json()
    assert report["title"] == "Polygon smoke" and not report["compatible"]

    course, contest = create("courses"), create("contests")
    course_path = f"/api/v1/courses/{course}"
    assert call("GET", course_path).json()["id"] == course
    call("POST", course_path + "/contests", 204, json={"contest_id": contest, "position": 0})
    assert call("GET", course_path + "/contests").json()["items"][0]["id"] == contest
    for url in (f"/courses/{course}", f"/problems/{problem}"):
        assert "text/html" in call("GET", url).headers["Content-Type"]

    contest_path = f"/api/v1/contests/{contest}"
    problems = contest_path + "/problems"
    call("POST", problems, 201, json={"problem_id": problem, "problem_revision_id": revision, "position": 0, "max_score": "100", "verdict_visibility": "summary"})
    full = archive("problem.xml", '<problem><names><name value="Full Polygon smoke"/></names></problem>', {"tests/01": "1 2\n", "tests/01.a": "3\n", "check.cpp": "// retained fixture\n"})
    imported = call("POST", problems + "/import/polygon", 201, files=full).json()
    cp = imported["contest_problem_id"]
    assert len(call("GET", problems).json()["items"]) == 2
    task = call("GET", f"{problems}/{cp}").json()
    assert task["problem_revision_id"] == imported["revision_id"]
    assert "python3" in [entry["code"] for entry in task["languages"]]
    call("PATCH", contest_path + "/settings", json={"status": "open", "self_registration": True, "scoreboard_visibility": "contest"})
    call("POST", contest_path + "/participants", 204, json={"self": True})
    submission = call("POST", f"{problems}/{cp}/submissions", 202, data={"language": "python3"}, files={"source": ("answer.py", b"print(3)\n")}).json()
    assert submission["problem_revision_id"] == imported["revision_id"]
    with call("GET", f"/api/v1/submissions/{submission['id']}/events", stream=True) as events:
        deadline = monotonic() + 12
        for line in events.iter_lines(chunk_size=1):
            if b'"verdict":"testing_error"' in line:
                break
            assert monotonic() < deadline, "No testing_error event within deadline"
        else:
            raise AssertionError("No testing_error event")
    attempt = call("GET", f"/api/v1/submissions/{submission['id']}/attempts").json()["items"][0]
    assert attempt["status"] == "error" and attempt["verdict"] == "testing_error"
    call("POST", "/auth/logout", 204)
    print(f"Authoring smoke passed: {prefix}")


if __name__ == "__main__":
    with requests.Session() as session:
        smoke(session)
