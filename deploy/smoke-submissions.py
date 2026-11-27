"""Verify submission visibility on a disposable dev stack with three Dex users.

Usage: PODTEST_SMOKE_URL=... python3 deploy/smoke-submissions.py
Requires smoke, owner and unrelated @podtest.test identities (same dev password).
Pass the disposable PostgreSQL container name to seed a completed attempt.
"""
import json
import runpy
import subprocess
import sys
from pathlib import Path

import requests

smoke = runpy.run_path(str(Path(__file__).with_name('smoke-authoring.py')))
login, archive = smoke['login_session'], smoke['archive']

with requests.Session() as manager_session, requests.Session() as owner_session, requests.Session() as stranger_session:
    manager = login(manager_session)
    owner = login(owner_session, 'owner@podtest.test')
    stranger = login(stranger_session, 'unrelated@podtest.test')
    participant = owner('GET', '/api/v1/me').json()['id']
    observer = stranger('GET', '/api/v1/me').json()['id']
    course = manager('POST', '/api/v1/courses', 201, json={'title': 'Course access smoke'}).json()['id']
    course_path = f'/api/v1/courses/{course}'
    owner('GET', course_path, 403)
    manager('PUT', f'{course_path}/members/{participant}/viewer', 204)
    assert any(item['user_id'] == participant and item['role'] == 'viewer' for item in manager('GET', course_path + '/members').json()['items'])
    contest = manager('POST', '/api/v1/contests', 201, json={'title': 'Visibility smoke'}).json()['id']
    cp = '/api/v1/contests/' + contest
    problem = manager('POST', '/api/v1/problems', 201, json={'title': 'Visibility fixture'}).json()['id']
    revs = f'/api/v1/problems/{problem}/revisions'
    revision = manager('POST', revs, 201, json={'execution_format': 'standard'}).json()['id']
    rev = f'{revs}/{revision}'
    manager('PUT', rev + '/statements/en', json={'source': '# Echo'})
    manager('PUT', rev + '/languages/posix_sh', 204)
    manifest = {'version': 1, 'language': 'posix_sh', 'build': 'package_source', 'checker': 'exact_bytes',
                'limits': {'wall_ms': 2000, 'output_bytes': 4096},
                'groups': [{'id': 'main', 'scoring': 'sum'}],
                'tests': [{'id': 'echo', 'group': 'main', 'input': 'tests/input', 'answer': 'tests/answer', 'points_milli': 1000}]}
    manager('PUT', rev + '/package', 204, files=archive('standard.json', json.dumps(manifest), {'tests/input': 'x', 'tests/answer': 'x'}))
    manager('POST', rev + '/publish', 204)
    task = manager('POST', cp + '/problems', 201, json={'problem_id': problem, 'problem_revision_id': revision,
                   'position': 0, 'max_score': '1', 'verdict_visibility': 'tests'}).json()['id']
    path = f'{cp}/problems/{task}'
    manager('PATCH', cp + '/settings', json={'status': 'open', 'self_registration': True, 'scoreboard_visibility': 'contest'})
    manager('POST', cp + '/participants', 204, json={'user_id': participant})
    source = b'printf "<fixture>"\n'
    sid = owner('POST', path + '/submissions', 202, data={'language': 'posix_sh'}, files={'source': ('source.txt', source)}).json()['id']
    sub = f'/api/v1/submissions/{sid}'
    for call in (manager, owner):
        response = call('GET', sub + '/source')
        assert response.content == source and response.headers['Cache-Control'] == 'private, no-store'
    for suffix in ('/source', '/attempts', '/events'):
        stranger('GET', sub + suffix, 403)
    owner('GET', path + '/submissions', 403)
    assert manager('GET', path + '/submissions').json()['items'][0]['id'] == sid
    assert owner('GET', path + '/submissions/me').json()['items'][0]['id'] == sid

    manager('PUT', f'{cp}/members/{observer}/viewer', 204)
    assert any(item['user_id'] == observer and item['role'] == 'viewer' for item in manager('GET', cp + '/members').json()['items'])
    stranger('GET', sub + '/source', 403)

    next_rev = manager('POST', revs, 201, json={'execution_format': 'standard', 'clone_from': revision}).json()['id']
    next_path = f'{revs}/{next_rev}'
    manager('PUT', next_path + '/statements/en', json={'source': '# Echo revision two'})
    manager('PUT', next_path + '/languages/posix_sh', 204)
    manager('POST', next_path + '/publish', 204)
    assert manager('GET', path).json()['problem_revision_id'] == revision
    manager('PATCH', path, 204, json={'problem_revision_id': next_rev})
    assert manager('GET', path).json()['problem_revision_id'] == next_rev
    assert owner('GET', sub).json()['submission']['problem_revision_id'] == revision

    if len(sys.argv) == 2:
        # Seed stored results to check HTTP access independently of worker timing.
        subprocess.run(['podman', 'exec', '-i', sys.argv[1], 'psql', '-U', 'podtest', '-d', 'podtest',
                        '-v', 'ON_ERROR_STOP=1', '-v', f'submission={sid}'], check=True, text=True, input=r"""
BEGIN;
INSERT INTO judging_attempts (submission_id, attempt_number, target_problem_revision_id, status, verdict, base_score, trigger_reason, max_score)
SELECT submission_id, attempt_number+1, target_problem_revision_id, 'finished', 'accepted', 1, 'visibility-fixture', max_score
FROM judging_attempts WHERE submission_id=:'submission' ORDER BY attempt_number DESC LIMIT 1 RETURNING id AS attempt \gset
INSERT INTO test_results VALUES (:'attempt','echo','accepted',1,'{"message":"manager only"}');
INSERT INTO subgroup_results VALUES (:'attempt','main',1);
COMMIT;
""")

        def sse(call):
            with call('GET', sub + '/events', stream=True) as response:
                for line in response.iter_lines(chunk_size=1):
                    if line.startswith(b'data: '):
                        return json.loads(line[6:])
            raise AssertionError('missing SSE snapshot')

        for visibility in ('tests', 'summary', 'hidden'):
            manager('PATCH', path, 204, json={'problem_revision_id': revision,
                    'settings': {'verdict_visibility': visibility, 'submission_limit': 2}})
            for call in (manager, owner):
                items = call('GET', sub + '/attempts').json()['items']
                assert items == sse(call)
                item = items[0]
                if call is manager:
                    assert item['protocol']['groups'][0]['subgroup_id'] == 'main'
                    assert item['tests'][0]['checker_metadata'] == {'message': 'manager only'}
                else:
                    assert not {'protocol', 'phase', 'active_tests', 'artifacts'} & item.keys()
                    if visibility == 'tests':
                        assert item['tests'] == [{'test_id': 'echo', 'verdict': 'accepted'}]
                        assert 'base_score' not in item and 'verdict' not in item
                    elif visibility == 'summary':
                        assert 'tests' not in item and item['verdict'] == 'accepted'
                    else:
                        assert not {'tests', 'verdict', 'base_score'} & item.keys()
    manager('PATCH', path, 204, json={'problem_revision_id': next_rev})
    owner('POST', sub + '/rejudge', 403, json={'problem_revision_id': next_rev})
    manager('POST', sub + '/rejudge', 409, json={'problem_revision_id': revision})
    rejudged = manager('POST', sub + '/rejudge', 202, json={'problem_revision_id': next_rev}).json()['judging_attempt_id']
    attempts = manager('GET', sub + '/attempts').json()['items']
    assert any(item['id'] == rejudged and item['protocol']['problem_revision_id'] == next_rev for item in attempts)
    print('Submission visibility smoke passed:', path.removeprefix('/api/v1'), sid)
