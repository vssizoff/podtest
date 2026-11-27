"""Smoke test against the running local dev stack (requires Python requests).

Creates uniquely named catalog entries; leaves them for inspection.
"""

from html.parser import HTMLParser
from io import BytesIO
from decimal import Decimal
import json
import os
import sys
from pathlib import Path
from time import monotonic, sleep
from urllib.parse import urljoin
from uuid import uuid4
from zipfile import ZipFile

import requests


BASE = os.environ.get("PODTEST_SMOKE_URL", "http://localhost:8088")


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


def login_session(s, email="smoke@podtest.test"):
    s.trust_env = False

    def call(method, path, status=200, **kwargs):
        for attempt in range(8):
            response = s.request(method, BASE + path, allow_redirects=False, timeout=15, **kwargs)
            if response.status_code != 429 or status == 429 or attempt == 7:
                break
            response.close()
            sleep(0.3)
        assert response.status_code == status, (method, path.split("?")[0], response.status_code, response.text)
        return response

    login = call("GET", "/auth/login", 302)
    dex = login.headers["Location"]
    assert dex.startswith(BASE + "/dex/")
    local_url = dex
    for _ in range(5):
        for attempt in range(8):
            page = s.get(local_url, allow_redirects=False, timeout=15)
            if page.status_code != 429 or attempt == 7:
                break
            page.close()
            sleep(0.3)
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
    callback = call("POST", action.removeprefix(BASE), 303, data={"login": email, "password": "podtest-local"})
    target = callback.headers["Location"]
    assert target.startswith(BASE + "/auth/callback?")
    call("GET", target.removeprefix(BASE), 303)
    session = call("GET", "/auth/session").json()
    assert session["authenticated"]
    s.headers.update({"Origin": BASE, "X-CSRF-Token": session["csrf_token"]})
    return call


def smoke(s):
    call = login_session(s)
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
    files = draft + "/package/files"
    assert call("GET", files, params={"name": "runner.lua"}).json()["text"] == "return {}\n"
    call("PATCH", files, 422, json={"name": "../escape", "text": "fixture"})
    call("PATCH", files, 204, json={"name": "runner.lua", "remove": True})
    call("POST", draft + "/validate", 422)
    call("PATCH", files, 204, json={"name": "runner.lua", "text": "return {}\n"})
    assert call("POST", draft + "/validate").json()["valid"]
    call("POST", draft + "/publish", 204)
    call("PATCH", files, 409, json={"name": "runner.lua", "text": "immutable"})
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
    lua = call("POST", problems, 201, json={"problem_id": problem, "problem_revision_id": revision, "position": 0, "max_score": "100", "verdict_visibility": "summary"}).json()["id"]
    full = archive("problem.xml", '<problem><names><name value="Full Polygon smoke"/></names></problem>', {"tests/01": "1 2\n", "tests/01.a": "3\n", "check.cpp": "// retained fixture\n"})
    imported = call("POST", problems + "/import/polygon", 201, files=full).json()
    cp = imported["contest_problem_id"]
    assert len(call("GET", problems).json()["items"]) == 2
    task = call("GET", f"{problems}/{cp}").json()
    assert task["problem_revision_id"] == imported["revision_id"]
    assert task["execution_available"] is False
    assert call("GET", f"{problems}/{lua}").json()["execution_available"] is False
    call("PATCH", contest_path + "/settings", json={"status": "open", "self_registration": True, "scoreboard_visibility": "contest"})
    call("POST", contest_path + "/participants", 204, json={"self": True})
    material = call("POST", "/api/v1/materials", 201, json={"title": prefix + " notes"}).json()["id"]
    revisions = f"/api/v1/materials/{material}/revisions"
    first = call("POST", revisions, 201, data={"format": "markdown", "source": "# First version"}, files={"file": ("notes.txt", b"fixture attachment")}).json()
    second = call("POST", revisions, 201, json={"format": "markdown", "source": "# Second version"}).json()
    assert [item["id"] for item in call("GET", revisions).json()["items"]] == [second["id"], first["id"]]
    call("POST", contest_path + "/materials", 204, json={"material_id": material, "material_revision_id": first["id"], "position": 0, "visibility": "managers"})
    pinned = call("GET", contest_path + "/materials").json()["items"][0]
    assert pinned["material_revision_id"] == first["id"] and "First version" in pinned["rendered_html"]
    assert call("GET", f"{contest_path}/materials/{material}/attachment").content == b"fixture attachment"
    for task_id in (cp, lua):
        path = f"{problems}/{task_id}/submissions"
        err = call("POST", path, 409, data={"language": "python3"}, files={"source": ("answer.py", b"print(3)\n")}).json()
        assert err["error"]["code"] == "execution_unavailable"
        assert call("GET", path + "/me").json()["items"] == []
    call("POST", "/auth/logout", 204)
    print(f"Authoring smoke passed: {prefix}")


def smoke_standard(s):
    call = login_session(s)
    title = "Standard smoke " + uuid4().hex[:8]
    problem = call("POST", "/api/v1/problems", 201, json={"title": title}).json()["id"]
    revisions = f"/api/v1/problems/{problem}/revisions"
    revision = call("POST", revisions, 201, json={"execution_format": "standard"}).json()["id"]
    draft = f"{revisions}/{revision}"
    call("PUT", draft + "/statements/en", json={"source": "# Echo the input"})
    call("PUT", draft + "/languages/posix_sh", 204)
    manifest = {
        "version": 1, "language": "posix_sh", "build": "package_source", "checker": "exact_bytes",
        "limits": {"wall_ms": 2000, "output_bytes": 4096},
        "groups": [{"id": "main", "scoring": "sum"}],
        "tests": [{"id": "echo", "group": "main", "input": "tests/input", "answer": "tests/answer", "points_milli": 1000}],
    }
    package = archive("standard.json", json.dumps(manifest), {"tests/input": "hello\n", "tests/answer": "hello\n"})
    call("PUT", draft + "/package", 204, files=package)
    files = draft + "/package/files"
    assert call("GET", files, params={"name": "tests/input"}).json()["text"] == "hello\n"
    call("PATCH", files, 422, json={"name": "../escape", "text": "fixture"})
    call("PATCH", files, 204, json={"name": "tests/input", "text": "edited\n"})
    assert call("GET", files, params={"name": "tests/input"}).json()["text"] == "edited\n"
    call("PATCH", files, 204, json={"name": "tests/input", "text": "hello\n"})
    call("PATCH", files, 204, json={"name": "notes.txt", "text": "temporary file"})
    call("PATCH", files, 204, json={"name": "notes.txt", "remove": True})
    assert "notes.txt" not in call("GET", files).json()["files"]
    validation = call("POST", draft + "/validate").json()
    assert validation["valid"], validation
    call("POST", draft + "/publish", 204)
    call("PATCH", files, 409, json={"name": "tests/input", "text": "immutable"})

    contest = call("POST", "/api/v1/contests", 201, json={"title": title}).json()["id"]
    path = f"/api/v1/contests/{contest}"
    task = call("POST", path + "/problems", 201, json={
        "problem_id": problem, "problem_revision_id": revision, "position": 0,
        "max_score": "1", "verdict_visibility": "tests",
    }).json()["id"]
    assert call("GET", f"{path}/problems/{task}").json()["execution_available"] is True
    call("PATCH", path + "/settings", json={"status": "open", "self_registration": True, "scoreboard_visibility": "contest"})
    call("POST", path + "/participants", 204, json={"self": True})
    submission = call("POST", f"{path}/problems/{task}/submissions", 202,
                      data={"language": "posix_sh"}, files={"source": ("echo.sh", b"exec cat\n")}).json()
    assert submission["problem_revision_id"] == revision, submission
    sid = submission["id"]
    print(f"Standard submission: {sid}", flush=True)
    deadline = monotonic() + 60
    attempt = None
    while monotonic() < deadline:
        attempts = call("GET", f"/api/v1/submissions/{sid}/attempts").json()["items"]
        attempt = attempts[0] if attempts else None
        if attempt and attempt.get("verdict"):
            break
        sleep(0.5)
    assert attempt and attempt["status"] == "finished" and attempt["verdict"] == "accepted", attempt
    assert Decimal(attempt["base_score"]) == 1, attempt
    assert [(test["test_id"], test["verdict"], Decimal(test["score"])) for test in attempt["tests"]] == [
        ("echo", "accepted", Decimal(1)),
    ], attempt
    call("POST", "/auth/logout", 204)
    print(f"Standard smoke passed: {title}; attempt {attempt['id']}, accepted, score {attempt['base_score']}")


def smoke_polygon_graphs(s, package):
    call = login_session(s)
    contest = call("POST", "/api/v1/contests", 201,
                   json={"title": "Testlib smoke " + uuid4().hex[:8]}).json()["id"]
    path = f"/api/v1/contests/{contest}"
    with package.open("rb") as archive_file:
        imported = call("POST", path + "/problems/import/polygon", 201,
                        files={"file": (package.name, archive_file, "application/zip")}).json()
    assert imported["report"]["compatible"], imported
    problem = call("GET", f"{path}/problems/{imported['contest_problem_id']}").json()
    assert {item["code"] for item in problem["languages"]} == {"javascript", "cpp23", "python314"}, problem
    call("PATCH", path + "/settings", json={"status": "open", "self_registration": True, "scoreboard_visibility": "contest"})
    call("POST", path + "/participants", 204, json={"self": True})
    target = f"{path}/problems/{imported['contest_problem_id']}/submissions"
    for language, filename, source, verdict, score in (
        ("javascript", "graphs.js", b"const k = +require('node:fs').readFileSync(0, 'utf8'); for (let i = 0; i < k; i++) console.log(`501 1\\n1 ${i + 2}`);", "accepted", 100),
        ("python314", "graphs.py", b"import sys\nassert sys.version_info[:2] == (3, 14)\nfor i in range(int(input())): print(f'501 1\\n1 {i + 2}')\n", "accepted", 100),
        ("cpp23", "graphs.cpp", b"#include <iostream>\nstatic_assert(__cplusplus >= 202302L);\nint main() { int k; std::cin >> k; for (int i = 0; i < k; ++i) std::cout << \"501 1\\n1 \" << i + 2 << '\\n'; }", "accepted", 100),
        ("python314", "loop.py", b"print('2 1\\n1 1')\n", "wrong_answer", 0),
        ("cpp23", "invalid.cpp", b"int main( {", "compilation_error", 0),
    ):
        submission = call("POST", target, 202, data={"language": language},
                          files={"source": (filename, source)}).json()
        sid = submission["id"]
        print(f"Testlib submission: {sid}; expecting {verdict}", flush=True)
        deadline = monotonic() + 180
        attempt = None
        while monotonic() < deadline:
            attempts = call("GET", f"/api/v1/submissions/{sid}/attempts").json()["items"]
            attempt = attempts[0] if attempts else None
            if attempt and attempt["status"] in ("finished", "error", "timeout"):
                break
            sleep(1)
        assert attempt and attempt["status"] == "finished" and attempt["verdict"] == verdict, attempt
        assert Decimal(attempt["base_score"]) == score, attempt
        print(f"Polygon smoke passed: {sid}, {language}, {verdict}, score {attempt['base_score']}", flush=True)
    call("POST", "/auth/logout", 204)


def smoke_testlib(s, package):
    with ZipFile(package) as source:
        header = source.read("files/testlib.h")
    call = login_session(s)
    title = "Testlib outcomes " + uuid4().hex[:8]
    contest = call("POST", "/api/v1/contests", 201, json={"title": title}).json()["id"]
    path = f"/api/v1/contests/{contest}"
    call("PATCH", path + "/settings", json={"status": "open", "self_registration": True, "scoreboard_visibility": "contest"})
    call("POST", path + "/participants", 204, json={"self": True})
    for position, (body, status, verdict, score) in enumerate((
        ('quitp(0.25, "fraction");', "finished", "partial", Decimal("0.25")),
        ('quit(_pc(40), "percentage");', "finished", "partial", Decimal("0.4")),
        ('quit(_pe, "format");', "finished", "presentation_error", Decimal(0)),
        ('quit(_fail, "jury failure");', "error", "testing_error", None),
        ('puts("not a testlib report"); fflush(stdout); std::_Exit(0);', "error", "testing_error", None),
        ('not valid C++;', "error", "testing_error", None),
    )):
        problem = call("POST", "/api/v1/problems", 201, json={"title": f"{title} {position}"}).json()["id"]
        revisions = f"/api/v1/problems/{problem}/revisions"
        revision = call("POST", revisions, 201, json={"execution_format": "standard"}).json()["id"]
        draft = f"{revisions}/{revision}"
        call("PUT", draft + "/statements/en", json={"source": "Testlib smoke fixture"})
        call("PUT", draft + "/languages/posix_sh", 204)
        manifest = {
            "version": 1, "language": "posix_sh", "build": "package_source", "checker": "testlib",
            "limits": {"wall_ms": 2000, "output_bytes": 4096},
            "groups": [{"id": "main", "scoring": "sum"}],
            "tests": [{"id": "one", "group": "main", "input": "tests/input", "answer": "tests/answer", "points_milli": 1000}],
        }
        cpp = ('#include "testlib.h"\nint main(int argc, char** argv) { '
               'registerTestlibCmd(argc, argv); ouf.readInt(); ' + body + '}\n')
        call("PUT", draft + "/package", 204, files=archive("standard.json", json.dumps(manifest), {
            "tests/input": "1\n", "tests/answer": "1\n", "checker/checker.cpp": cpp, "checker/testlib.h": header,
        }))
        call("POST", draft + "/publish", 204)
        task = call("POST", path + "/problems", 201, json={
            "problem_id": problem, "problem_revision_id": revision, "position": position,
            "max_score": "1", "verdict_visibility": "tests",
        }).json()["id"]
        sid = call("POST", f"{path}/problems/{task}/submissions", 202, data={"language": "posix_sh"},
                   files={"source": ("echo.sh", b"exec cat\n")}).json()["id"]
        deadline = monotonic() + 120
        attempt = None
        while monotonic() < deadline:
            items = call("GET", f"/api/v1/submissions/{sid}/attempts").json()["items"]
            attempt = items[0] if items else None
            if attempt and attempt["status"] in ("finished", "error", "timeout"):
                break
            sleep(0.5)
        assert attempt and attempt["status"] == status and attempt["verdict"] == verdict, (body, attempt)
        if score is None:
            assert attempt.get("base_score") is None and not attempt.get("tests"), attempt
        else:
            assert Decimal(attempt["base_score"]) == score, attempt
            assert [(t["verdict"], Decimal(t["score"])) for t in attempt["tests"]] == [(verdict, score)], attempt
        print(f"Testlib smoke passed: {sid}, {body} -> {verdict}, score {score}", flush=True)
    call("POST", "/auth/logout", 204)


if __name__ == "__main__":
    args = sys.argv[1:]
    with requests.Session() as session:
        if len(args) == 2 and args[0] == "--polygon-graphs":
            smoke_polygon_graphs(session, Path(args[1]))
        elif len(args) == 2 and args[0] == "--testlib":
            smoke_testlib(session, Path(args[1]))
        elif args in ([], ["--standard"]):
            (smoke_standard if args else smoke)(session)
        else:
            raise SystemExit(f"Usage: {sys.argv[0]} [--standard | --polygon-graphs ZIP | --testlib ZIP]")
