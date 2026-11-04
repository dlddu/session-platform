// tools/journey-prototype.test.mjs
//
// 여정 mockup(docs/journeys/<journey-id>/index.html)이 사용자 여정 문서와 1:1인지,
// 그리고 그 페이지가 "클릭되는 제품 프로토타입"인지를 실제 브라우저에서 집행한다.
//
//   node --test tools/journey-prototype.test.mjs
//
// 왜 정적 대조가 아니라 브라우저인가:
//   파일을 읽어 속성만 세면 배선이 끊긴 버튼과 살아 있는 버튼이 구분되지 않는다.
//   화면 안의 행동으로 전진하는가 · 입력이 진짜 동작하는가 · 정상 경로 밖 상태에
//   도달하는가는 DOM 을 굴려야만 알 수 있다.
//
// 규약 정의는 docs/journeys/README.md. 규약을 바꾸면 이 파일도 같은 커밋에서 바꾼다.

import { before, after, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { chromium } from '@playwright/test';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const DOCS = path.join(ROOT, 'docs');
const JOURNEY_DOC_DIR = path.join(DOCS, 'user-journeys');
const PAGE_ROOT = path.join(DOCS, 'journeys');
const MAPPING_FILE = path.join(DOCS, 'mockups', 'README.md');
const REGISTRY_FILE = path.join(DOCS, 'doc-structure-state.md');
const HUB_FILE = path.join(DOCS, 'index.html');

const FONT_HOSTS = ['fonts.googleapis.com', 'fonts.gstatic.com'];
// 2026-08-30 순번 → 슬러그 전환으로 폐기된 식별자. 새 대상에 재사용하면 drift.
const RETIRED_ID = /\bJ[1-9](-S[0-9]+)?\b/;

const read = (p) => fs.readFileSync(p, 'utf8');

/* ────────────────────────────── 문서 파싱 ────────────────────────────── */

/** 여정 문서 하나 → { id, steps[], branches[] } */
function parseJourneyDoc(file) {
  const src = read(file);
  const id = path.basename(file, '.md');

  // 단계 헤딩은 백틱 코드스팬 안에 있다: "### `STP-…` 제목"
  // `^### STP-` 로 세면 0건이 나온다 — 이 함정 때문에 정규식을 고정한다.
  const steps = [...src.matchAll(/^###\s+`(STP-[a-z0-9-]+)`/gm)].map((m) => m[1]);

  // "## 4. 분기·예외 흐름" 표의 세 번째 열("이어지는 단계")
  const sec = src.match(/^##\s*4\.[^\n]*\n([\s\S]*?)(?=\n##\s|(?![\s\S]))/m);
  const branches = [];
  if (sec) {
    const rows = sec[1]
      .split('\n')
      .map((l) => l.trim())
      .filter((l) => l.startsWith('|') && !/^\|\s*-+/.test(l));
    // 첫 행은 헤더
    for (const row of rows.slice(1)) {
      const cells = row.split('|').slice(1, -1).map((c) => c.trim());
      if (cells.length < 3) continue;
      branches.push({ situation: cells[0], targets: canonicalTargets(cells[2]) });
    }
  }
  return { id, steps, branches, file };
}

/**
 * "이어지는 단계" 셀 → 허용되는 data-branch-target 값의 집합.
 *   `STP-x`                         → { 'STP-x' }
 *   `JRN-y`의 `STP-x`               → { 'JRN-y#STP-x' }
 *   `JRN-y` · `JRN-z` (단계 없음)   → { 'JRN-y', 'JRN-z' }
 *   해당 없음                        → { 'none' }
 */
function canonicalTargets(cell) {
  if (/해당\s*없음/.test(cell)) return new Set(['none']);
  const tokens = [...cell.matchAll(/`(JRN-[a-z0-9-]+|STP-[a-z0-9-]+)`/g)].map((m) => ({
    id: m[1],
    at: m.index,
  }));
  const steps = tokens.filter((t) => t.id.startsWith('STP-'));
  const journeys = tokens.filter((t) => t.id.startsWith('JRN-'));
  const out = new Set();
  if (steps.length) {
    for (const s of steps) {
      const owner = journeys.filter((j) => j.at < s.at).pop();
      out.add(owner ? `${owner.id}#${s.id}` : s.id);
    }
  } else {
    for (const j of journeys) out.add(j.id);
  }
  return out;
}

/** doc-structure-state.md "수용된 위험" → [{ id, hasReviewDate }] */
function parseExceptions() {
  const src = read(REGISTRY_FILE);
  const sec = src.match(/^##\s*수용된 위험\s*\n([\s\S]*?)(?=\n##\s|(?![\s\S]))/m);
  if (!sec) return [];
  const items = sec[1].split(/\n(?=- )/).filter((s) => s.trim().startsWith('- '));
  return items
    .map((item) => {
      const m = item.match(/`(JRN-[a-z0-9-]+)`/);
      // "재검토" 라는 낱말이 서술 어딘가에 스쳐도 통과하면 검사가 헐거워진다.
      // 규칙 8이 요구하는 것은 *재검토 시점* 이라는 항목 자체이므로 그 표기를 요구한다.
      return m ? { id: m[1], hasReviewDate: /재검토\s*시점/.test(item), text: item } : null;
    })
    .filter(Boolean);
}

/** mockups/README.md "여정 → 여정 mockup 페이지 매핑" 표 → Map<journeyId, {steps, status, path}> */
function parseMapping() {
  const src = read(MAPPING_FILE);
  const sec = src.match(/^##\s*여정 → 여정 mockup 페이지 매핑[^\n]*\n([\s\S]*?)(?=\n##\s|(?![\s\S]))/m);
  assert.ok(sec, `${path.relative(ROOT, MAPPING_FILE)} 에 "여정 → 여정 mockup 페이지 매핑" 섹션이 없다`);
  const out = new Map();
  for (const line of sec[1].split('\n')) {
    const l = line.trim();
    if (!l.startsWith('|') || /^\|\s*-+/.test(l)) continue;
    const cells = l.split('|').slice(1, -1).map((c) => c.trim());
    const jm = cells[0] && cells[0].match(/`(JRN-[a-z0-9-]+)`/);
    if (!jm) continue; // 헤더 행
    const row = cells.join(' | ');
    const status = row.includes('✅') ? 'page' : row.includes('⚪') ? 'excepted' : row.includes('⏳') ? 'pending' : null;
    out.set(jm[1], {
      steps: new Set([...row.matchAll(/`(STP-[a-z0-9-]+)`/g)].map((m) => m[1])),
      status,
      raw: row,
    });
  }
  return out;
}

/**
 * mockups/README.md "단계별 화면·상태 목록" 표 → [{journey, step, screen, states[]}]
 * states 는 정렬된 배열이다 — 표의 표기 순서와 무관하게 집합으로 비교한다.
 */
function parseScreenIndex() {
  const src = read(MAPPING_FILE);
  const sec = src.match(/^##\s*단계별 화면·상태 목록[^\n]*\n([\s\S]*?)(?=\n##\s|(?![\s\S]))/m);
  assert.ok(sec, `${path.relative(ROOT, MAPPING_FILE)} 에 "단계별 화면·상태 목록" 섹션이 없다`);
  const rows = [];
  for (const line of sec[1].split('\n')) {
    const l = line.trim();
    if (!l.startsWith('|') || /^\|\s*-+/.test(l)) continue;
    const cells = l.split('|').slice(1, -1).map((c) => c.trim());
    if (cells.length < 4) continue;
    const jm = cells[0].match(/`(JRN-[a-z0-9-]+)`/);
    const st = cells[1].match(/`(STP-[a-z0-9-]+)`/);
    const sc = cells[2].match(/`([a-z0-9-]+)`/);
    if (!jm || !st || !sc) continue; // 헤더 행
    rows.push({
      journey: jm[1],
      step: st[1],
      screen: sc[1],
      states: [...cells[3].matchAll(/`([a-z0-9-]+)`/g)].map((m) => m[1]).sort(),
    });
  }
  assert.ok(rows.length > 0, '"단계별 화면·상태 목록" 표에 데이터 행이 없다');
  return rows;
}

/** mockups/README.md "의도적 제외" 섹션 → [{line, screen, journey}] */
function parseIntentionalOmissions() {
  const src = read(MAPPING_FILE);
  const sec = src.match(/^##\s*의도적 제외[^\n]*\n([\s\S]*?)(?=\n##\s|(?![\s\S]))/m);
  assert.ok(sec, `${path.relative(ROOT, MAPPING_FILE)} 에 "의도적 제외" 섹션이 없다`);
  const entries = [];
  for (const line of sec[1].split('\n')) {
    const l = line.trim();
    if (!l.startsWith('- ')) continue;
    entries.push({
      line: l,
      screen: (l.match(/^- \*\*화면 `([a-z0-9-]+)`\*\*/) || [])[1] || null,
      journey: (l.match(/^- \*\*여정 `(JRN-[a-z0-9-]+)`\*\*/) || [])[1] || null,
    });
  }
  assert.ok(entries.length > 0, '"의도적 제외" 섹션에 등재가 하나도 없다');
  return entries;
}

/**
 * 여정 페이지 DOM → 화면 인벤토리 실측.
 *
 * 화면 표면(.screen)의 "그림"은 표면 자체 + 그 뒤 구역(다음 .screen 직전까지)이다.
 * 규칙 4의 분기 결과 메모([data-state-note])처럼 화면 뒤에 붙는 장치는 그 단계의
 * 마지막 화면에 속한다 — 그래서 한 단계에 화면이 둘이어도 상태가 새지 않는다.
 */
async function readScreenFacts(page) {
  return page.evaluate(() => {
    const facts = {
      inventory: [],
      steps: [],
      screenIds: [],
      blankScreen: [],
      strayScreen: [],
      orphanState: [],
      orphanOffpath: [],
      offpathNav: [],
      stepsWithoutScreen: [],
    };
    const journeyEl = document.querySelector('[data-journey]');
    if (!journeyEl) return facts;
    const journey = journeyEl.getAttribute('data-journey');

    // data-screen 은 화면 표면에만 쓴다.
    for (const el of document.querySelectorAll('[data-screen]')) {
      if (!el.classList.contains('screen')) facts.strayScreen.push(el.outerHTML.slice(0, 100));
    }

    for (const step of document.querySelectorAll('section[data-step]')) {
      const screens = [...step.querySelectorAll('.screen')];
      facts.steps.push({ journey, step: step.dataset.step });
      if (screens.length === 0) facts.stepsWithoutScreen.push(`${journey} ${step.dataset.step}`);

      const owned = new Map(screens.map((s) => [s, new Set()]));
      for (const el of step.querySelectorAll('[data-state]')) {
        const own = el.closest('.screen');
        if (own) {
          owned.get(own).add(el.getAttribute('data-state'));
          continue;
        }
        const prior = screens.filter(
          (s) => (s.compareDocumentPosition(el) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0
        );
        const target = prior[prior.length - 1];
        if (target && el.hasAttribute('data-state-note')) {
          owned.get(target).add(el.getAttribute('data-state'));
        } else {
          facts.orphanState.push(`${journey} ${step.dataset.step}: ${el.outerHTML.slice(0, 100)}`);
        }
      }

      for (const s of screens) {
        const id = s.getAttribute('data-screen');
        facts.screenIds.push(id);
        if (!id || !id.trim()) {
          facts.blankScreen.push(`${journey} ${step.dataset.step}: ${s.outerHTML.slice(0, 100)}`);
        }
        facts.inventory.push({
          journey,
          step: step.dataset.step,
          screen: id,
          states: [...owned.get(s)].sort(),
        });
      }
    }

    for (const el of document.querySelectorAll('[data-offpath]')) {
      if (!el.closest('.screen')) facts.orphanOffpath.push(el.outerHTML.slice(0, 100));
      if (el.hasAttribute('data-advance-to') || el.hasAttribute('data-branch-row')) {
        facts.offpathNav.push(el.outerHTML.slice(0, 100));
      }
    }
    return facts;
  });
}

/* ────────────────────────────── 수집 ────────────────────────────── */
const journeys = fs
  .readdirSync(JOURNEY_DOC_DIR)
  .filter((f) => /^JRN-[a-z0-9-]+\.md$/.test(f))
  .sort()
  .map((f) => parseJourneyDoc(path.join(JOURNEY_DOC_DIR, f)));

const journeyById = new Map(journeys.map((j) => [j.id, j]));

const pageDirs = fs.existsSync(PAGE_ROOT)
  ? fs
      .readdirSync(PAGE_ROOT, { withFileTypes: true })
      .filter((d) => d.isDirectory())
      .map((d) => d.name)
      .filter((n) => fs.existsSync(path.join(PAGE_ROOT, n, 'index.html')))
      .sort()
  : [];

const hasPage = (id) => pageDirs.includes(id);
const exceptions = parseExceptions();
const exceptedIds = new Set(exceptions.map((e) => e.id));
const mapping = parseMapping();
const hubSrc = read(HUB_FILE);

let browser;
before(async () => {
  browser = await chromium.launch();
});
after(async () => {
  if (browser) await browser.close();
});

/* ────────────────────────── 문서 층위 (규칙 1·6·7·8) ────────────────────────── */

describe('여정 문서 파싱', () => {
  it('여정과 단계를 읽어낸다', () => {
    assert.ok(journeys.length > 0, 'docs/user-journeys 에 JRN-*.md 가 없다');
    for (const j of journeys) {
      assert.ok(j.steps.length > 0, `${j.id}: "### \`STP-…\`" 단계 헤딩이 0개로 파싱됐다`);
      assert.equal(new Set(j.steps).size, j.steps.length, `${j.id}: 단계 식별자가 중복된다`);
    }
  });
});

describe('예외 등재 (규칙 8)', () => {
  it('등재된 여정은 실재하고, 사유와 재검토 시점을 모두 갖는다', () => {
    for (const e of exceptions) {
      assert.ok(journeyById.has(e.id), `수용된 위험에 등재된 ${e.id} 가 여정 문서에 없다`);
      assert.ok(
        e.hasReviewDate,
        `${e.id} 예외 등재에 재검토 시점이 없다 — 규칙 8은 사유와 재검토 시점을 함께 요구한다`
      );
    }
  });

  it('예외 등재된 여정은 페이지를 갖지 않는다', () => {
    for (const id of exceptedIds) {
      assert.ok(!hasPage(id), `${id} 는 비시각화로 등재됐는데 페이지가 있다 — 등재를 걷거나 페이지를 지운다`);
    }
  });
});

describe('매핑 SSOT (규칙 7) — docs/mockups/README.md', () => {
  it('모든 판정 대상 여정이 정확히 한 행으로 선언돼 있다', () => {
    for (const j of journeys) {
      assert.ok(
        mapping.has(j.id),
        `${j.id} 가 매핑 표에 없다 — 페이지가 없는 여정도 "⏳ 예정" 또는 "⚪ 예외"로 선언해야 한다(침묵 금지)`
      );
    }
    for (const id of mapping.keys()) {
      assert.ok(journeyById.has(id), `매핑 표의 ${id} 에 해당하는 여정 문서가 없다`);
    }
  });

  it('행의 단계 목록이 여정 문서의 단계 집합과 같다', () => {
    for (const j of journeys) {
      const row = mapping.get(j.id);
      assert.deepEqual(
        [...row.steps].sort(),
        [...j.steps].sort(),
        `${j.id}: 매핑 표의 단계 목록이 여정 문서와 다르다`
      );
    }
  });

  it('행의 상태가 실제 상태와 일치한다', () => {
    for (const j of journeys) {
      const { status } = mapping.get(j.id);
      assert.ok(status, `${j.id}: 매핑 표 상태 표기(✅/⏳/⚪)가 없다`);
      if (status === 'page') {
        assert.ok(hasPage(j.id), `${j.id}: 매핑은 ✅ 인데 docs/journeys/${j.id}/index.html 이 없다`);
      } else if (status === 'excepted') {
        assert.ok(exceptedIds.has(j.id), `${j.id}: 매핑은 ⚪ 인데 doc-structure-state.md 에 등재가 없다`);
      } else {
        assert.ok(!hasPage(j.id), `${j.id}: 매핑은 ⏳ 예정인데 페이지가 이미 있다 — ✅ 로 올려야 한다`);
        assert.ok(!exceptedIds.has(j.id), `${j.id}: 예외 등재된 여정을 ⏳ 예정으로 두었다`);
      }
    }
  });
});

describe('고아 페이지 (규칙 2)', () => {
  it('모든 페이지 디렉터리가 실재하는 여정이다', () => {
    for (const dir of pageDirs) {
      assert.ok(journeyById.has(dir), `docs/journeys/${dir}/ 에 대응하는 여정 문서가 없다(고아 페이지)`);
    }
  });
});

/* ──────────────────────── 화면 식별자 축 (규약 10) ──────────────────────── */

describe('화면 식별자 (규약 10)', () => {
  const facts = {
    inventory: [],
    steps: [],
    screenIds: new Set(),
    blankScreen: [],
    strayScreen: [],
    orphanState: [],
    orphanOffpath: [],
    offpathNav: [],
    stepsWithoutScreen: [],
  };

  before(async () => {
    for (const id of pageDirs) {
      const p = await browser.newPage();
      await p.goto(pathToFileURL(path.join(PAGE_ROOT, id, 'index.html')).href);
      const f = await readScreenFacts(p);
      facts.inventory.push(...f.inventory);
      facts.steps.push(...f.steps);
      for (const s of f.screenIds) facts.screenIds.add(s);
      facts.blankScreen.push(...f.blankScreen);
      facts.strayScreen.push(...f.strayScreen);
      facts.orphanState.push(...f.orphanState);
      facts.orphanOffpath.push(...f.orphanOffpath);
      facts.offpathNav.push(...f.offpathNav);
      facts.stepsWithoutScreen.push(...f.stepsWithoutScreen);
      await p.close();
    }
  });

  it('모든 화면 표면이 화면 식별자를 노출한다', () => {
    assert.ok(facts.inventory.length > 0, '화면 표면이 하나도 수집되지 않았다');
    assert.deepEqual(facts.blankScreen, [], '화면 표면(.screen) 중 data-screen 이 비어 있거나 없는 것이 있다');
    assert.deepEqual(
      facts.strayScreen,
      [],
      'data-screen 을 단 요소 중 .screen 이 아닌 것이 있다 — 화면 식별자는 화면 표면에만 쓴다'
    );
  });

  it('상태·여정 밖 행동은 화면 표면 안에만 있다', () => {
    assert.deepEqual(
      facts.orphanState,
      [],
      '화면 밖(또는 화면 뒤 구역이 아닌 곳)의 data-state 가 있다 — 규칙 4 의 분기 결과 메모([data-state-note])만 예외다'
    );
    assert.deepEqual(
      facts.orphanOffpath,
      [],
      '화면 밖의 data-offpath 가 있다 — 여정 밖 행동도 그리는 화면 안에 있어야 한다'
    );
    assert.deepEqual(
      facts.offpathNav,
      [],
      'data-offpath 를 단 요소가 data-advance-to · data-branch-row 를 함께 갖는다 — 여정 밖 행동은 전진도 분기도 아니다'
    );
  });

  it('단계별 화면·상태 목록이 DOM과 같다', () => {
    const index = parseScreenIndex();
    const key = (r) => `${r.journey}|${r.step}|${r.screen}|${[...r.states].sort().join(',')}`;
    assert.deepEqual(
      facts.inventory.map(key).sort(),
      index.map(key).sort(),
      'mockups/README.md "단계별 화면·상태 목록" 이 DOM과 다르다 — DOM이 정본이다'
    );
    // 침묵한 누락: 단계도 화면도 인덱스에 반드시 실려 있어야 한다.
    assert.deepEqual(facts.stepsWithoutScreen, [], '화면 표면이 하나도 없는 단계가 있다');
    const indexSteps = new Set(index.map((r) => `${r.journey}|${r.step}`));
    for (const s of facts.steps) {
      assert.ok(
        indexSteps.has(`${s.journey}|${s.step}`),
        `${s.journey} ${s.step} 가 "단계별 화면·상태 목록" 에 없다 — 침묵한 누락`
      );
    }
    const indexScreens = new Set(index.map((r) => `${r.journey}|${r.step}|${r.screen}`));
    for (const r of facts.inventory) {
      assert.ok(
        indexScreens.has(`${r.journey}|${r.step}|${r.screen}`),
        `${r.journey} ${r.step} 의 화면 ${r.screen} 가 "단계별 화면·상태 목록" 에 없다`
      );
    }
  });

  it('의도적 제외 등재는 형식을 갖추고 그리지 않은 대상만 든다', () => {
    for (const e of parseIntentionalOmissions()) {
      assert.ok(e.line.includes('근거:'), `의도적 제외 등재에 근거가 없다: ${e.line}`);
      assert.ok(e.line.includes('사유:'), `의도적 제외 등재에 사유가 없다: ${e.line}`);
      assert.ok(e.screen || e.journey, `의도적 제외 등재 형식이 아니다(화면/여정 식별자가 없다): ${e.line}`);
      if (e.screen) {
        assert.ok(
          !facts.screenIds.has(e.screen),
          `화면 ${e.screen} 를 "의도적 제외" 에 등재했는데 DOM에 그려져 있다 — 등재를 걷는다`
        );
      }
      if (e.journey) {
        assert.ok(
          !hasPage(e.journey),
          `여정 ${e.journey} 를 "의도적 제외" 에 등재했는데 페이지가 있다 — 등재를 걷는다`
        );
        assert.ok(
          exceptedIds.has(e.journey),
          `여정 ${e.journey} 는 doc-structure-state.md "수용된 위험" 에 등재돼 있지 않다 — 예외 등재 없이는 의도적 제외가 아니다`
        );
      }
    }
  });
});

describe('허브 동기화 (규칙 7) — docs/index.html', () => {
  for (const j of journeys) {
    it(`${j.id} 행이 실제 상태를 가리킨다`, () => {
      const anchor = `user-journeys/${j.id}.md`;
      const at = hubSrc.indexOf(anchor);
      assert.ok(at >= 0, `허브에 ${j.id} 문서 링크가 없다`);
      const rowEnd = hubSrc.indexOf('</div>', at);
      const row = hubSrc.slice(at, rowEnd);
      if (hasPage(j.id)) {
        assert.ok(
          row.includes(`href="journeys/${j.id}/"`),
          `허브의 ${j.id} 행이 아직 플레이스홀더다 — journeys/${j.id}/ 로 갱신해야 한다`
        );
      } else {
        assert.ok(row.includes('class="missing"'), `허브의 ${j.id} 행이 없는 페이지를 링크한다`);
        if (exceptedIds.has(j.id)) {
          assert.ok(
            /비시각화/.test(row),
            `허브의 ${j.id} 행이 "준비 중"으로 보인다 — 예외 등재 여정은 의도적 비시각화로 표기해야 한다`
          );
        }
      }
    });
  }
});

/* ────────────────────────── 페이지 층위 (규칙 2~6) ────────────────────────── */

if (pageDirs.length === 0) {
  describe('여정 페이지', () => {
    it('아직 없다 — 문서 층위 검사만 수행했다', () => {
      assert.ok(true);
    });
  });
}

for (const id of pageDirs) {
  const journey = journeyById.get(id);
  if (!journey) continue; // 고아 페이지는 위에서 이미 실패로 잡힌다

  const file = path.join(PAGE_ROOT, id, 'index.html');
  const src = read(file);
  const url = pathToFileURL(file).href;

  describe(`여정 페이지 ${id}`, () => {
    let page;
    let external;

    const open = async (hash) => {
      external = [];
      page = await browser.newPage();
      // file: 요청은 가로채지 않는다(가로채면 로컬 페이지 로드 자체가 흔들린다).
      // 장식용 폰트를 포함해 바깥으로 나가는 요청은 전부 막고, 그래도 동작하는지 본다.
      await page.route(
        (u) => u.protocol !== 'file:',
        (route) => {
          const u = route.request().url();
          if (!FONT_HOSTS.some((h) => u.includes(h))) external.push(u);
          return route.abort();
        }
      );
      await page.goto(hash ? `${url}#${hash}` : url);
      return page;
    };

    const closePage = async () => {
      if (page) {
        await page.close();
        page = undefined;
      }
    };

    it('(규칙 8h) 정적으로 동작한다 — 모듈·네트워크 API 를 쓰지 않는다', () => {
      assert.ok(!/<script[^>]+type=["']module["']/.test(src), '<script type="module"> 은 file:// 에서 막힌다');
      assert.ok(!/\bfetch\s*\(/.test(src), 'fetch() 는 file:// 에서 쓸 수 없다');
      assert.ok(!/XMLHttpRequest/.test(src), 'XMLHttpRequest 는 file:// 에서 쓸 수 없다');
      assert.ok(!/\bimport\s*\(/.test(src), '동적 import() 는 file:// 에서 막힌다');
    });

    it('(규칙 6) 폐기된 순번 식별자를 재사용하지 않는다', () => {
      const body = src.replace(/<!--[\s\S]*?-->/g, '');
      const hit = body.match(RETIRED_ID);
      assert.equal(hit, null, `폐기 식별자 "${hit && hit[0]}" 가 페이지에 남아 있다`);
    });

    it('(규칙 6) 존재하지 않는 여정·단계 식별자를 참조하지 않는다', () => {
      const known = new Set([...journeyById.keys(), ...journeys.flatMap((j) => j.steps)]);
      for (const m of src.matchAll(/\b(JRN|STP)-[a-z0-9-]+/g)) {
        assert.ok(known.has(m[0]), `알 수 없는 식별자 ${m[0]} 를 참조한다`);
      }
    });

    it('(규칙 2) 여정을 정확히 하나 선언한다', async () => {
      await open();
      assert.equal(await page.locator('[data-journey]').count(), 1);
      assert.equal(await page.locator('[data-journey]').getAttribute('data-journey'), id);
      await closePage();
    });

    it('(규칙 3) 단계 집합이 여정 문서와 양방향으로 같다', async () => {
      await open();
      const onPage = await page.$$eval('[data-step]', (els) => els.map((e) => e.getAttribute('data-step')));
      assert.deepEqual(onPage, journey.steps, '단계 집합(순서 포함)이 여정 문서와 다르다');
      const ids = await page.$$eval('[data-step]', (els) => els.map((e) => e.id));
      assert.deepEqual(ids, journey.steps, '각 단계 section 의 id 가 data-step 과 같아야 딥링크가 선다');
      await closePage();
    });

    it('(규칙 5b) 문서 메타가 기본으로 접혀 있다', async () => {
      await open();
      for (const step of journey.steps) {
        const meta = page.locator(`[data-step="${step}"] details.step-meta`);
        assert.equal(await meta.count(), 1, `${step}: details.step-meta 가 정확히 1개여야 한다`);
        assert.equal(await meta.getAttribute('open'), null, `${step}: 단계 메타가 열린 채로 시작한다`);
      }
      await closePage();
    });

    it('(규칙 5f) 각 단계에 딥링크로 바로 진입한다', async () => {
      for (const step of journey.steps) {
        await open(step);
        assert.equal(await page.locator('[data-journey]').getAttribute('data-step-active'), step);
        await page.locator(`[data-step="${step}"]`).waitFor({ state: 'visible' });
        const visible = await page.$$eval('[data-step]', (els) => els.filter((e) => !e.hidden).length);
        assert.equal(visible, 1, `${step}: 동시에 보이는 단계가 1개가 아니다`);
        await closePage();
      }
    });

    it('(규칙 5c) 화면 안의 행동만으로 처음부터 끝까지 전진한다', async () => {
      await open();
      for (let i = 0; i < journey.steps.length - 1; i += 1) {
        const step = journey.steps[i];
        const next = journey.steps[i + 1];
        const advance = page.locator(`[data-step="${step}"] [data-advance-to="${next}"]`);
        assert.equal(
          await advance.count(),
          1,
          `${step}: 그 단계 화면 안에 [data-advance-to="${next}"] 가 정확히 1개 있어야 한다(래퍼 네비 금지)`
        );
        await advance.first().click();
        assert.equal(
          await page.locator('[data-journey]').getAttribute('data-step-active'),
          next,
          `${step} → ${next} 전진이 배선되지 않았다`
        );
      }
      await closePage();
    });

    it('(규칙 5g) 마지막 단계가 여정의 끝을 표현한다', async () => {
      const last = journey.steps[journey.steps.length - 1];
      await open(last);
      const end = page.locator(`[data-step="${last}"] [data-journey-end]`);
      assert.equal(await end.count(), 1, `${last}: [data-journey-end] 가 없다`);
      assert.ok(await end.first().isVisible());
      await closePage();
    });

    it('(규칙 5d) 표시된 입력이 실제 폼 요소이고 동작한다', async () => {
      await open();
      const required = await page.$$eval('[data-required-input]', (els) =>
        els.map((e, i) => ({
          index: i,
          tag: e.tagName,
          type: e.getAttribute('type') || '',
          step: e.closest('[data-step]').dataset.step,
        }))
      );
      await closePage();

      assert.ok(required.length > 0, '[data-required-input] 로 표시된 입력이 하나도 없다');
      for (const inp of required) {
        assert.ok(
          ['INPUT', 'SELECT', 'TEXTAREA'].includes(inp.tag),
          `${inp.step}: ${inp.tag} 는 실제 폼 요소가 아니다 — 입력처럼 보이는 비대화형 요소는 규칙 5d 위반`
        );
        await open(inp.step);
        const el = page.locator('[data-required-input]').nth(inp.index);
        await el.waitFor({ state: 'visible' });
        if (inp.tag === 'INPUT' && (inp.type === 'radio' || inp.type === 'checkbox')) {
          await el.check();
          assert.equal(await el.isChecked(), true, `${inp.step}: 선택이 반영되지 않는다`);
        } else if (inp.tag === 'SELECT') {
          const values = await el.evaluate((s) => Array.from(s.options).map((o) => o.value));
          assert.ok(values.length > 1, `${inp.step}: select 에 고를 값이 없다`);
          // 그 단계에서 아직 못 고르게 막아 둔 select 는 비활성 자체가 의도된 상태다.
          if (await el.isEnabled()) {
            await el.selectOption(values[1]);
            assert.equal(await el.inputValue(), values[1], `${inp.step}: 선택이 반영되지 않는다`);
          }
        } else {
          await el.focus();
          assert.ok(
            await el.evaluate((e) => document.activeElement === e),
            `${inp.step}: 입력에 포커스가 가지 않는다`
          );
          await el.fill('');
          await el.pressSequentially('probe-1');
          assert.equal(await el.inputValue(), 'probe-1', `${inp.step}: 타이핑이 반영되지 않는다`);
        }
        await closePage();
      }
    });

    it('(규칙 4·5e) 분기표의 모든 행이 배선돼 있다', async () => {
      await open();
      const wired = await page.$$eval('[data-branch-target]', (els) =>
        els.map((e) => ({
          row: Number(e.getAttribute('data-branch-row')),
          target: e.getAttribute('data-branch-target'),
          pending: e.hasAttribute('data-branch-pending'),
          excepted: e.hasAttribute('data-branch-excepted'),
          openWith: e.getAttribute('data-branch-open'),
          step: e.closest('[data-step]').dataset.step,
        }))
      );
      assert.equal(
        wired.length,
        journey.branches.length,
        `분기 요소 ${wired.length}개 ≠ 여정 문서 "4. 분기·예외 흐름" 표 ${journey.branches.length}행`
      );
      const rows = wired.map((w) => w.row).sort((a, b) => a - b);
      assert.deepEqual(
        rows,
        journey.branches.map((_, i) => i + 1),
        'data-branch-row 는 1..N 을 빠짐없이 한 번씩 써야 한다'
      );
      for (const w of wired) {
        const spec = journey.branches[w.row - 1];
        assert.ok(
          spec.targets.has(w.target),
          `분기 ${w.row}행("${spec.situation}"): data-branch-target="${w.target}" 가 문서의 대상 ${[...spec.targets].join(' | ')} 와 다르다`
        );
        if (w.target.startsWith('JRN-')) {
          const targetJourney = w.target.split('#')[0];
          if (hasPage(targetJourney)) {
            assert.ok(
              !w.pending,
              `분기 ${w.row}행: ${targetJourney} 페이지가 이미 있다 — data-branch-pending 을 걷고 실제 링크로 승급해야 한다`
            );
          }
          // 미승급 갈래는 왜 페이지가 없는지가 원장과 일치해야 한다.
          // "아직 안 만든 것"(⏳ 예정)과 "그리지 않기로 한 것"(⚪ 예외 등재)은 다른 상태이고,
          // 뒤엣것은 영원히 pending 이므로 표시 없이 두면 미완으로 읽힌다.
          if (w.excepted) {
            assert.ok(
              exceptedIds.has(targetJourney),
              `분기 ${w.row}행: data-branch-excepted 는 예외 등재 여정에만 붙인다 — ${targetJourney} 는 doc-structure-state.md 에 등재돼 있지 않다`
            );
            assert.ok(
              w.pending,
              `분기 ${w.row}행: 예외 등재 여정(${targetJourney})은 페이지가 생기지 않으므로 data-branch-pending 을 함께 달아야 한다`
            );
          } else if (w.pending) {
            const row = mapping.get(targetJourney);
            assert.equal(
              row && row.status,
              'pending',
              `분기 ${w.row}행: ${targetJourney} 를 "아직 페이지 없음"으로 표시했는데 매핑 표의 상태가 ⏳ 예정이 아니다 — 예외 등재라면 data-branch-excepted 를 붙인다`
            );
          }
        }
      }
      await closePage();
    });

    for (const [i, spec] of journey.branches.entries()) {
      it(`(규칙 4) ${i + 1}행 "${spec.situation}" 을 눌러 선언한 곳으로 간다`, async () => {
        const sel = `[data-branch-row="${i + 1}"]`;
        await open();
        const meta = await page.$eval(sel, (e) => ({
          step: e.closest('[data-step]').dataset.step,
          openWith: e.getAttribute('data-branch-open'),
          target: e.getAttribute('data-branch-target'),
          pending: e.hasAttribute('data-branch-pending'),
        }));
        await closePage();

        await open(meta.step);
        if (meta.openWith) {
          await page.locator(`#${meta.openWith}`).click();
        }
        const el = page.locator(sel);
        await el.waitFor({ state: 'visible' });
        await el.click();

        if (meta.target.startsWith('STP-')) {
          assert.equal(
            await page.locator('[data-journey]').getAttribute('data-step-active'),
            meta.target,
            `같은 여정 안의 분기는 실제로 그 단계로 이동해야 한다`
          );
        } else if (meta.target.startsWith('JRN-') && !meta.pending) {
          // 승급된 갈래 — 대상 페이지가 실재하므로 눌러서 실제로 그 페이지의 그 단계에 도착해야 한다.
          // 여기까지 확인해야 "pending 을 걷었다"가 링크가 살아 있다는 뜻이 된다(페이지 간 딥링크 무결성).
          const [targetJourney, targetStep] = meta.target.split('#');
          const expected = pathToFileURL(path.join(PAGE_ROOT, targetJourney, 'index.html')).href;
          await page.waitForURL((u) => u.href.split('#')[0] === expected, { timeout: 10_000 });
          assert.equal(
            await page.locator('[data-journey]').getAttribute('data-journey'),
            targetJourney,
            `분기 ${i + 1}행: 승급된 갈래가 ${targetJourney} 페이지로 데려가지 않았다`
          );
          if (targetStep) {
            assert.equal(
              await page.locator('[data-journey]').getAttribute('data-step-active'),
              targetStep,
              `분기 ${i + 1}행: ${targetJourney} 페이지가 ${targetStep} 로 열리지 않았다 — 페이지 간 딥링크가 끊겼다`
            );
          }
        } else {
          // 대상 페이지가 아직 없는 갈래(또는 "해당 없음") — 결과가 화면에 남아야 한다
          const note = page.locator(`[data-step="${meta.step}"] [data-state-note]`);
          assert.ok(
            await note.isVisible(),
            `분기 ${i + 1}행: 눌렀을 때 [data-state-note] 로 결과를 알려야 한다`
          );
        }
        await closePage();
      });
    }

    it('(규칙 5h) 폰트 CDN을 포함한 외부 요청 없이 동작한다', async () => {
      await open();
      const lastStep = journey.steps[journey.steps.length - 1];
      await page.goto(`${url}#${lastStep}`);
      assert.deepEqual(external, [], `외부 자원을 요청한다: ${external.join(', ')}`);
      await closePage();
    });
  });
}
