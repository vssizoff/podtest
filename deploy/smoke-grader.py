"""Verify the custom Grader and explicit score publication on a disposable dev stack."""
import json
import runpy
from decimal import Decimal
from pathlib import Path
from time import monotonic, sleep

import requests


def main():
    smoke = runpy.run_path(str(Path(__file__).with_name("smoke-authoring.py")))
    with requests.Session() as session:
        call = smoke["login_session"](session)
        title = "Custom Grader smoke"
        problem = call("POST", "/api/v1/problems", 201, json={"title": title}).json()["id"]
        revisions = f"/api/v1/problems/{problem}/revisions"
        revision = call("POST", revisions, 201, json={
            "execution_format": "standard", "grader_mode": "manual",
        }).json()["id"]
        draft = f"{revisions}/{revision}"
        call("PUT", draft + "/statements/en", json={"source": "# Echo"})
        call("PUT", draft + "/languages/posix_sh", 204)
        manifest = {"version": 1, "language": "posix_sh", "build": "package_source", "checker": "exact_bytes",
                    "limits": {"wall_ms": 2000, "output_bytes": 4096},
                    "groups": [{"id": "main", "scoring": "sum"}],
                    "tests": [{"id": "echo", "group": "main", "input": "tests/input", "answer": "tests/answer", "points_milli": 1000}]}
        call("PUT", draft + "/package", 204, files=smoke["archive"]("standard.json", json.dumps(manifest), {"tests/input": "hello\n", "tests/answer": "hello\n"}))
        call("PATCH", draft + "/package/files", 204, json={"name": "grader.py", "text": '''def grade(snapshot):
    problem = snapshot["run"]["contest_problem_id"]
    return {"log": "custom grader executed", "scores": [
        {"participant_id": participant, "contest_problem_id": problem, "score": "0.75"}
        for participant in snapshot["participants"]
    ]}
'''})
        call("POST", draft + "/publish", 204)
        contest = call("POST", "/api/v1/contests", 201, json={"title": title}).json()["id"]
        path = f"/api/v1/contests/{contest}"
        task = call("POST", path + "/problems", 201, json={
            "problem_id": problem, "problem_revision_id": revision, "position": 0,
            "max_score": "1", "verdict_visibility": "tests",
        }).json()["id"]
        call("PATCH", path + "/settings", json={"status": "open", "self_registration": True, "scoreboard_visibility": "contest"})
        call("POST", path + "/participants", 204, json={"self": True})
        create = f"{path}/problems/{task}/grader-runs"
        run = call("POST", create, 202, json={"mode": "preview"}).json()["id"]
        detail = f"/api/v1/grader-runs/{run}"
        end = monotonic() + 45
        while True:
            result = call("GET", detail).json()
            if result["status"] in ("failed", "completed") or monotonic() >= end:
                break
            sleep(0.5)
        assert result["status"] == "completed" and not result["published"], result
        assert result["log"] == "custom grader executed" and Decimal(result["scores"][0]["score"]) == Decimal("0.75"), result
        assert Decimal(call("GET", path + "/scoreboard").json()["rows"][0]["total"]) == 0
        publication = call("POST", detail + "/publish").json()
        assert call("POST", detail + "/publish").json() == publication
        assert call("GET", detail).json()["published"]
        assert Decimal(call("GET", path + "/scoreboard").json()["rows"][0]["scores"][task]) == Decimal("0.75")
        print(f"Grader smoke passed: /contests/{contest} /problems/{problem}")


if __name__ == "__main__":
    main()
