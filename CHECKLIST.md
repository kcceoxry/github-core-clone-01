# CHECKLIST pra-submit — github-core-clone-01 (Fase 5)

Tanggal: 19 November 2026 ~24:00 WIB. Deadline bounty: 21 November 2026 13:30 WIB.
Repo lokal: `~/workspace/makefun/gitforge-build/` (git init sudah, 3 commit).
Repo GitHub Bos: BELUM ADA — username GitHub Bos belum diketahui.

## 1. Determinisme — LULUS

- Sealed env (`program/env/server.mjs` + `main.ts`, MAKEFUN_STATE_DIR) dijalankan
  3x pada instance yang sama (run1, run2, run3).
- `export/state.json` ketiga run BYTE-IDENTIK, sha256:
  `a0e98d49854b961230227867235c8615858e006db3763f341ac6154af1445590`
  (`diff` run1 vs run2 kosong; run3 dengan binary statis baru juga identik).
- Bukti di repo (di-.gitignore, ada di disk): `det-run1/2/3-state.json`,
  `det-run1/2/3-perf.json`, dan `run-det.sh` (script yang menjalankannya).
- perf.json WAJAR berbeda antar run (hanya timing): run1 191 poin, run2 196,
  run3 191. Timing memang hanya tinggal di perf.json, bukan di state.json.
- Kode: `merged_at` memakai counter operasi (`opTimestampString`), bukan jam
  dinding; tidak ada `time.Now` / `rand.` di kode non-test; serialisasi state
  mengurutkan key (lolos 120/120 tiga kali).

## 2. Kit check penuh terakhir — LULUS (120/120)

- `kit-run-phase5.json` (sdk/run.ts --perf --json, binary statis baru):
  120/120 checks, 15/15 MCP, grup git 30/30, repos 15/15, issues 15/15,
  pulls 20/20, protocol 5/5, graphql 20/20, mcp 15/15.
- Perf lokal: 197/200 (push 50, clone 50, read 49, write 48),
  perf_p50_us 181506. Total lokal = 600 + 200 + 197 = 997.
- Catatan: grade-local CLI (`--driver docker`) TIDAK BISA dijalankan di mesin
  ini karena Docker tidak terpasang. Sebagai gantinya, env tersegel yang sama
  dijalankan langsung (program/env/server.mjs + MAKEFUN_STATE_DIR) — itu
  mesin yang dipakai grade-local di balik layar.

## 3. Binary — LULUS (<= 8 MiB)

- `forge-server`: 6.652.088 byte (6,34 MiB), static linked, stripped,
  dibangun dari source sesi ini dengan
  `CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o forge-server ./cmd/server`.
- Keputusan packaging: binary DI-COMMIT ke repo (bukan build saat runtime),
  karena trial pod kemungkinan tidak punya Go dan RULES mengizinkan binary
  statis yang di-commit sebagai vendored source. `main.ts` meluncurkan
  `./forge-server` di sebelahnya.
- Tree total repo: 6.963.685 byte (< 16 MiB). File terbesar: forge-server.
- `gitproto.test` (11 MB) TIDAK di-commit (.gitignore) — di atas batas 8 MiB.

## 4. Repo lokal + init — LULUS (anchor tercatat)

- `git init -b main`, 3 commit:
  - `2eecc3b` "makefun anchor: drand round 34101466" (root, .makefun/freshness.json)
  - `b4e9b4c` "LICENSE: MIT" (SPDX MIT)
  - `c4fd632` "Forge: Go git forge, 120/120 checks, static binary, README + determinism evidence"
- `makefun init` BERHASIL (setelah MAKEFUN_CREDENTIALS dialihkan ke file
  sementara, karena `~/.makefun/credentials` hasil agent-connect tidak
  memenuhi validasi CLI — lihat butir 4b). Anchor yang dicetak:
  **makefun anchor 2eecc3b0551b101ac596ecd1f516b347bba70f76 round 34101466**
- freshness.json: drand-quicknet round 34101466 (> round_min 33944812),
  wallet AyNhJYKjMLfMC8axzKi27qQhsyFUh6Nf2aurcULZBkfb (agent wallet, terhubung
  ke akun @sniper-elite).
- 4b. PERINGATAN — jangan ubah `~/.makefun/credentials` (milik parent, berisi
  API key akun yang terhubung). CLI menolaknya karena entrinya hanya punya
  `key` + `expires_at`, sementara CLI menuntut `wallet`, `user_id`, `handle`,
  `saved_at`. Solusi yang dipakai: `MAKEFUN_CREDENTIALS=/tmp/makefun-creds-init.json`
  untuk perintah init saja. Perintah submit nanti akan menemui masalah yang
  sama bila dijalankan dari CLI — parent harus memutuskan: perbaiki format
  file kredensial (tambah field yang hilang dari data akun yang terverifikasi)
  atau jalankan submit dengan MAKEFUN_CREDENTIALS terpisah.
- thinking_fresh: anchor di atas dikutip di tool result SESI INI (Fase 5).
  Sesi Fase 1–4 menulis kode SEBELUM anchor ada. Untuk entri repo GitHub,
  thinking_fresh tidak berlaku. Untuk entri source bundle, check ini hanya
  "skip as a note" (bukan penolakan). Semua sesi build berikutnya HARUS
  mengutip anchor di atas SEBELUM write pertama yang dihitung.

## 5. Bundle --dry-run — LULUS (dengan 3 hal yang harus dilengkapi parent)

- Perintah: `node cli/makefun.mjs bundle --repo <build dir> --out /tmp/bundle-dry.tar.gz --dry-run`
- Hasil: 6 session file Muse app DITEMUKAN, semuanya recorded (BUKAN handed-over):
  - 08f90468 (95 turns, 350 baris)
  - 0fa9c12e (169 turns, 529 baris)
  - 3973425c — subagent Fase 1 (78 turns, 252 baris)
  - 3aefd0cf (58 turns, 201 baris)
  - 64b6c6fb — agen utama/parent (99 turns, 317 baris)
  - 73a0b14c — subagent Fase 5 / sesi ini (30 turns, 95 baris)
- Instruction files: AGENTS.md (dari ~/AGENTS.md) + system prompt
  (~/workspace/system/system_prompt.md) — status: uploaded.
- Run record: prompt (typed) terisi dari sesi utama.
- Redaksi: 15 email, 585 home path, 113 username, 4 credential dihapus
  otomatis oleh bundle (bukan oleh agen).
- YANG MASIH HARUS DILENGKAPI PARENT (agen top-level, SETELAH subagent ini
  selesai — baris usage subagent baru ditulis saat run selesai):
  a. `--muse-usage <file>`: salin baris usage dari muse.db untuk keenam
     agent_id di atas (SELECT di bawah), satu objek JSON per baris, lalu
     bundle ulang. Tanpa ini entri DITOLAK (min 1 usage row untuk sesi Muse app).
     SELECT: `SELECT u.agent_id AS agent_id, u.message_id AS message_id,
     u.created_at AS created_at, u.input_tokens AS input_tokens,
     u.output_tokens AS output_tokens, a.model AS model FROM
     agent.agent_message_token_usage u JOIN agent.agents a ON
     a.agent_id = u.agent_id WHERE u.agent_id IN ('<agent_id>', ...)
     ORDER BY u.created_at DESC LIMIT 500`
  b. MCP servers: sesi tidak mencatat MCP apa yang dipakai. Bila memang tidak
     ada yang dipakai, tambahkan `--mcp none`.
  c. Agent facts: tambahkan `--harness muse-app --harness-version <versi>
     --provider meta --model muse-spark-1.3 --effort default`
     (model muse-spark-1.3 sesuai RULES untuk build aplikasi Muse; parent
     mohon verifikasi harness-version dari log aplikasi bila ada).
  d. Prompt run record: dry-run mengisi prompt dengan kalimat Bos
     "ok kita akan buat ini study penting ..." dari sesi utama. Parent mohon
     periksa apakah itu prompt yang benar untuk run record, atau isi dengan
     --prompt-file berisi kata-kata pembuka Bos untuk build ini.
- Bundle ASLI (bukan dry-run) belum dibuat — harus dibuat parent setelah
  (a)–(d) di atas, karena usage row sesi ini baru ada setelah run ini selesai.

## 6. README + About — SEBAGIAN (butuh username GitHub Bos)

- README.md di repo mengikuti persis `how_to_submit.readme_template` dari
  requirements JSON (3 badge live make.fun). Badge rank/reward masih berisi
  placeholder `GITHUB-OWNER` — HARUS diganti username GitHub Bos sebelum submit.
- Lisensi: MIT (SPDX), dicetak init.
- About GitHub (website https://make.fun/bounty/github-core-clone-01 + topik
  makefun,ai-agents,bounties,code): BELUM BISA — butuh repo GitHub.
- Repo dibuat setelah 14 Nov 06:30 UTC: repo lokal dibuat 19 Nov 2026
  (anchor round 34101466, drand). Repo GitHub nanti harus dibuat setelah
  tanggal itu juga (otomatis terpenuhi bila dibuat sekarang).

## 7. No secret di history — LULUS

- Diperiksa: tidak ada API key (`af_...`), private key, token, atau password
  di file yang di-commit. Satu-satunya kredensial yang pernah tampil (API key
  make.fun di `~/.makefun/credentials`) TIDAK ada di repo dan TIDAK boleh
  ditulis ke file mana pun.
- `git log` hanya berisi 3 commit di atas; tidak ada file `.env`/kunci.

## 8. Slot submisi — TERSEDIA

- Belum ada submisi apa pun untuk bounty ini. Kuota: maks 5/hari, 10 total.
  Simpan 1 slot untuk jam-jam terakhir (house masuk di akhir).

## TODO yang butuh username GitHub Bos (urut)

1. Username GitHub Bos → buat repo publik (nama disarankan
   `github-core-clone-01`), set About (website + topik makefun) SEBELUM submit.
2. Ganti `GITHUB-OWNER` di README.md dengan username itu, commit, push.
3. Push history lokal (root 2eecc3b …) ke repo GitHub — jangan rewrite history.
4. Parent: buat bundle ASLI dengan (a)–(d) butir 5 di atas.
5. Submit SATU entri (repo GitHub, tanpa --keypair — hadiah tidak di-escrow):
   `node cli/makefun.mjs submit --competition github-core-clone-01
   --repo-url https://github.com/<owner>/github-core-clone-01 --commit <sha>
   --title "Forge — GitHub-compatible git forge" --bundle transcripts.tar.gz`
   (atau submit source bundle `--source <repo> --keypair <file>` bila tanpa
   GitHub — perlu anchor dikutip sesi, lihat butir 4).
6. Cek status: `node cli/makefun.mjs status --submission <id>`.
7. JANGAN submit sebelum butir 1–4 selesai; entri terakhir sebelum deadline
   yang dihitung.
