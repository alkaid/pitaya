# Upstream review ledger

This ledger pins the upstream delta used by the fork audit. It is an inventory
and priority queue, not a claim that all upstream behavior has been accepted.

- fork merge base: `5230ff3dc79b461f55b6193bb9d0ae5c3d13869f`
  (`v2.11.1`);
- reviewed upstream branch tip:
  `45717a2f25acc3419b9f94ef1f9fbc4c14d13e7c` (`v2.11.24`);
- graph delta: 70 commits, comprising 66 non-merge commits and four merge
  commits;
- patch-id result at this snapshot: 66 upstream-only patches and no patch-id
  equivalents on `main`.

The category list was expanded from the original five audit buckets. Collapsing
features, CI, maintenance, and observability into correctness would make the
priority queue misleading. Security and correctness remain the only automatic
priority classes.

## Status contract

| Status | Meaning |
| --- | --- |
| `unreviewed` | Classified from the diff, but no fork disposition is approved |
| `reviewed-pending` | The fork still lacks verified behavior; a fork-native patch is required |
| `blocked` | Review found a dependency on an unresolved design decision |
| `in-progress` | A separately owned port is being implemented |
| `ported` | Reimplemented for the fork with a fork ref and verification evidence |
| `cherry-picked` | Applied directly with a fork ref and verification evidence |
| `already-equivalent` | Current fork behavior supersedes the patch, with evidence |
| `rejected` | Reviewed and intentionally not adopted, with a reason |
| `covered-by-parents` | Merge topology only; its parent patches are tracked separately |

`git cherry` is only patch evidence. It cannot prove semantic equivalence after
the fork's App, Session, RPC, executor, and lifecycle changes. A terminal status
therefore requires current-code evidence and a verification command.

## Priority queue

1. `d0a71b1`: etcd username/password are absent from Viper defaults; review as a
   security configuration batch.
2. `ddc9083`, `37fa8dc`, `cca67c0`, and `45717a2`: the public/internal `appDie`
   ownership chain can send on a channel closed by App shutdown; treat the train
   as one lifecycle design batch.
3. `4ab87be`: NATS request handling dereferences a nil message.
4. `5c06377`: unique-session kick failure can leave an old session registered.
5. `89e6d09` and `b524a30`: etcd revoke/watch timeout and revision recovery need
   fork-native lifecycle tests.
6. `c37f8a0`, `955929e`, and `38f56ae`: error payload and write-timeout semantics
   need serializer/compression-aware tests.
7. `4f5c74b`: StatsD histogram reporting still returns `ErrNotImplemented`.
8. `9d0bc4a` and `86abbf6`: blocked by the separate F-18 metric naming and
   collector ownership decision.

None of these entries authorizes a runtime change in this documentation batch.
Each port must be isolated, adapted to current fork invariants, tested against
its base, documented, and committed separately.

## Inventory

| Upstream commit | Date | Category | Priority | Status | Assessment |
| --- | --- | --- | --- | --- | --- |
| [112a146fd0ef0ea203b33a6a10597478f9a22ba7](https://github.com/topfreegames/pitaya/commit/112a146fd0ef0ea203b33a6a10597478f9a22ba7) | 2024-06-25 | observability | P3 | unreviewed | Adds logs for internally generated errors; not semantically reviewed. |
| [42732da613f80b9241a7e8a2cc26834626cbdf4c](https://github.com/topfreegames/pitaya/commit/42732da613f80b9241a7e8a2cc26834626cbdf4c) | 2024-06-25 | observability | P3 | unreviewed | Adds logging on blind spots; not semantically reviewed. |
| [5434a8e18594b9b71db1ca5a398c3f0cfd85cdaa](https://github.com/topfreegames/pitaya/commit/5434a8e18594b9b71db1ca5a398c3f0cfd85cdaa) | 2024-06-25 | observability | P3 | unreviewed | Differentiates pending writes for diagnostics; not semantically reviewed. |
| [7f2020c935d660d5f1a6e6a27b9af82e346c3ea5](https://github.com/topfreegames/pitaya/commit/7f2020c935d660d5f1a6e6a27b9af82e346c3ea5) | 2024-06-25 | correctness | P2 | unreviewed | Error reporting fixes in an early tracing patch train; review with the whole train. |
| [29fbc088c0db992f8e6c3bf5bcce6acd8141f9cd](https://github.com/topfreegames/pitaya/commit/29fbc088c0db992f8e6c3bf5bcce6acd8141f9cd) | 2024-06-25 | observability | P3 | unreviewed | Temporary timing logging; not semantically reviewed. |
| [c5ef742f5052ecd36e9e1d565430b1af34f3aed2](https://github.com/topfreegames/pitaya/commit/c5ef742f5052ecd36e9e1d565430b1af34f3aed2) | 2024-06-25 | observability | P3 | unreviewed | Adds a missing diagnostic log; not semantically reviewed. |
| [96a2eed11e9f179402b65b745e590631fe9af842](https://github.com/topfreegames/pitaya/commit/96a2eed11e9f179402b65b745e590631fe9af842) | 2024-06-25 | observability | P3 | unreviewed | Logs context for pending-write failure; not semantically reviewed. |
| [498443d40b1e5e9d18e50b560040b8d640a38df8](https://github.com/topfreegames/pitaya/commit/498443d40b1e5e9d18e50b560040b8d640a38df8) | 2024-06-25 | correctness | P2 | unreviewed | Wraps low-level connection errors; review with fork error-code semantics. |
| [49dca9b6cecb9b54b848c5cb25270dc64b77b6cc](https://github.com/topfreegames/pitaya/commit/49dca9b6cecb9b54b848c5cb25270dc64b77b6cc) | 2024-06-25 | observability | P3 | unreviewed | Adds connection-write tracing spans; not semantically reviewed. |
| [fc23e827c8666b44bc554794d787f42cbc81a57f](https://github.com/topfreegames/pitaya/commit/fc23e827c8666b44bc554794d787f42cbc81a57f) | 2024-06-25 | correctness | P2 | unreviewed | Handles empty context messages; review with current propagation code. |
| [9cf2c74f1bc37adf173678101154bee7953722be](https://github.com/topfreegames/pitaya/commit/9cf2c74f1bc37adf173678101154bee7953722be) | 2024-06-25 | correctness | P2 | unreviewed | Fixes a missing context update; review with current propagation code. |
| [bc54743b11e6d0f21e229e682b149ecd7d920511](https://github.com/topfreegames/pitaya/commit/bc54743b11e6d0f21e229e682b149ecd7d920511) | 2024-06-25 | correctness | P2 | unreviewed | Fixes trace span parent selection; not semantically reviewed. |
| [8855add7be3afee624f59986648379e76c81625f](https://github.com/topfreegames/pitaya/commit/8855add7be3afee624f59986648379e76c81625f) | 2024-06-25 | maintenance | P3 | unreviewed | Simplifies tracing code; not semantically reviewed. |
| [12e1baa7c7adddeb702bbd701c3060bd3a453edd](https://github.com/topfreegames/pitaya/commit/12e1baa7c7adddeb702bbd701c3060bd3a453edd) | 2024-06-25 | correctness | P2 | unreviewed | Adds a connection-write error and tracing wrapper; review with fork errors. |
| [12186770550089eaeeeb01ac862b76a3631e71a9](https://github.com/topfreegames/pitaya/commit/12186770550089eaeeeb01ac862b76a3631e71a9) | 2024-06-25 | maintenance | P3 | unreviewed | Cleanup in the tracing patch train; not independently reviewable. |
| [d2aa56f3d58195eef49131eb91a884d4f57ee704](https://github.com/topfreegames/pitaya/commit/d2aa56f3d58195eef49131eb91a884d4f57ee704) | 2024-06-25 | correctness | P2 | unreviewed | Finishes base spans correctly; review with current tracing lifecycle. |
| [92e1ceb91f94d1ae3ffad01832beb34e17c87bf3](https://github.com/topfreegames/pitaya/commit/92e1ceb91f94d1ae3ffad01832beb34e17c87bf3) | 2024-06-25 | maintenance | P3 | unreviewed | Code cleanup; not semantically reviewed. |
| [c423231d2e2deb78be28e592b28cd167ae971d7d](https://github.com/topfreegames/pitaya/commit/c423231d2e2deb78be28e592b28cd167ae971d7d) | 2024-06-25 | observability | P3 | unreviewed | Removes duplicate span error logging; not semantically reviewed. |
| [12fef523a6d53c4948ce13324f0c45383ae56600](https://github.com/topfreegames/pitaya/commit/12fef523a6d53c4948ce13324f0c45383ae56600) | 2024-06-25 | maintenance | P3 | unreviewed | Simplifies the tracing patch train; not semantically reviewed. |
| [2fd5550233cc21c7ad09861c4d6999f31c1f8ce7](https://github.com/topfreegames/pitaya/commit/2fd5550233cc21c7ad09861c4d6999f31c1f8ce7) | 2024-06-25 | test | P3 | unreviewed | Updates upstream unit tests; fork applicability not reviewed. |
| [1d1cfe9a5410822b376e205735e15a9aa8d5be30](https://github.com/topfreegames/pitaya/commit/1d1cfe9a5410822b376e205735e15a9aa8d5be30) | 2024-06-25 | test | P2 | unreviewed | Covers connection send-write failure; candidate regression for error review. |
| [11965de806b78b5373beed2987559ff879bdd2b6](https://github.com/topfreegames/pitaya/commit/11965de806b78b5373beed2987559ff879bdd2b6) | 2024-06-25 | test | P2 | unreviewed | Asserts write-failure error code; candidate regression for error review. |
| [68da1bc72f196ec766a837731fcfa53af725e05a](https://github.com/topfreegames/pitaya/commit/68da1bc72f196ec766a837731fcfa53af725e05a) | 2024-06-25 | maintenance | P3 | unreviewed | Removes deprecated io/ioutil usage; current fork impact not reviewed. |
| [89e6d09e64528c7274214566d546987e160732b6](https://github.com/topfreegames/pitaya/commit/89e6d09e64528c7274214566d546987e160732b6) | 2024-07-08 | correctness | P1 | reviewed-pending | required: replace context.TODO revoke flow with bounded ownership and add timeout/panic regressions. |
| [fa9721e7f191a97ec306badc702a2ae170677bb0](https://github.com/topfreegames/pitaya/commit/fa9721e7f191a97ec306badc702a2ae170677bb0) | 2024-08-06 | observability | P3 | unreviewed | Improves connection error logs; not semantically reviewed. |
| [50c6877730eb08a68109cf116cb71ca89f03d5a0](https://github.com/topfreegames/pitaya/commit/50c6877730eb08a68109cf116cb71ca89f03d5a0) | 2024-08-19 | maintenance | P3 | covered-by-parents | parents: fa9721e7f191a97ec306badc702a2ae170677bb0 and 89e6d09e64528c7274214566d546987e160732b6. |
| [c37f8a02ee38c2dd6ab88e73f3e73c9ddf8b2e14](https://github.com/topfreegames/pitaya/commit/c37f8a02ee38c2dd6ab88e73f3e73c9ddf8b2e14) | 2024-12-12 | correctness | P1 | reviewed-pending | required: make payload error decoding serializer-aware and add JSON/protobuf/custom serializer tests. |
| [3eb2a530441aeb24924c6175b5c3d261eecd970e](https://github.com/topfreegames/pitaya/commit/3eb2a530441aeb24924c6175b5c3d261eecd970e) | 2025-01-14 | feature | P2 | unreviewed | Adds NATS compression configuration; compare with current fork message compression. |
| [be642be525fd09e2d4e9e07702217d2449ebabb0](https://github.com/topfreegames/pitaya/commit/be642be525fd09e2d4e9e07702217d2449ebabb0) | 2025-01-14 | feature | P2 | unreviewed | Ports connection information behavior; fork session boundary not reviewed. |
| [76aaccde0414ed7034923e76d59962685fd44942](https://github.com/topfreegames/pitaya/commit/76aaccde0414ed7034923e76d59962685fd44942) | 2025-01-16 | feature | P2 | unreviewed | Adds same-server loopback RPC; review against fork routing semantics. |
| [b4ca5869247b8e44966809b98e278efb6bd8aa31](https://github.com/topfreegames/pitaya/commit/b4ca5869247b8e44966809b98e278efb6bd8aa31) | 2025-01-16 | test | P3 | unreviewed | Fixes tests in the loopback patch train; not independently reviewed. |
| [38f56aeae016fd0c192994a3fb138f5206b721b9](https://github.com/topfreegames/pitaya/commit/38f56aeae016fd0c192994a3fb138f5206b721b9) | 2025-01-21 | correctness | P2 | reviewed-pending | required: replace the fork's hard-coded write timeout with an explicit compatible config contract and deadline tests. |
| [2bbd948f930d5b9788da309f15d0bcc2c586cee0](https://github.com/topfreegames/pitaya/commit/2bbd948f930d5b9788da309f15d0bcc2c586cee0) | 2025-01-21 | feature | P2 | unreviewed | Adds NATS connection options; review option ownership and startup behavior. |
| [85f76155aed215197d4df44281bd414b0e739e11](https://github.com/topfreegames/pitaya/commit/85f76155aed215197d4df44281bd414b0e739e11) | 2025-01-27 | correctness | P1 | unreviewed | Waits for reconnect during setup; current constructor needs an executable comparison test. |
| [f0f83adc03dd307a07b416b51d5fbf2719e7e0fe](https://github.com/topfreegames/pitaya/commit/f0f83adc03dd307a07b416b51d5fbf2719e7e0fe) | 2025-02-05 | dependency | P2 | already-equivalent | fork-ref: 0791d29; verify: go.mod selects nats.go v1.52.0 and make module-verify checks the graph. |
| [e4f68e547d3a512a72439358a1d7b78fc0f9a2ae](https://github.com/topfreegames/pitaya/commit/e4f68e547d3a512a72439358a1d7b78fc0f9a2ae) | 2025-02-05 | build/ci | P3 | unreviewed | Runs upstream tests on v2; current fork CI is structurally different. |
| [6e28829bd6199bd648cb28477a32422cc35b275d](https://github.com/topfreegames/pitaya/commit/6e28829bd6199bd648cb28477a32422cc35b275d) | 2025-02-05 | build/ci | P3 | unreviewed | Fixes upstream v2 CI; compare only through explicit current lane evidence. |
| [661a1e71591f929b9363c9f7b4a951068121c558](https://github.com/topfreegames/pitaya/commit/661a1e71591f929b9363c9f7b4a951068121c558) | 2025-02-05 | build/ci | P3 | unreviewed | Fixes upstream go.work; current multi-module workspace is independently governed. |
| [b367da2f41940392036fab4572936e7eafcd6f9d](https://github.com/topfreegames/pitaya/commit/b367da2f41940392036fab4572936e7eafcd6f9d) | 2025-02-05 | test | P3 | unreviewed | Fixes upstream tests; fork applicability not reviewed. |
| [289b20491695473a10734db3ada3d712c6eb42f4](https://github.com/topfreegames/pitaya/commit/289b20491695473a10734db3ada3d712c6eb42f4) | 2025-02-05 | correctness | P1 | unreviewed | Disables async retry on initial connect; current behavior needs a contract test before disposition. |
| [319b45615de740ab8dc4e1ad2017802bb0a264a5](https://github.com/topfreegames/pitaya/commit/319b45615de740ab8dc4e1ad2017802bb0a264a5) | 2025-02-07 | correctness | P1 | unreviewed | Drains NATS connections on error; review with current process and shutdown ownership. |
| [bbd02fa6c3992d11314b46441065a48824ce83f7](https://github.com/topfreegames/pitaya/commit/bbd02fa6c3992d11314b46441065a48824ce83f7) | 2025-03-14 | maintenance | P3 | covered-by-parents | parents: b4ca5869247b8e44966809b98e278efb6bd8aa31 and 319b45615de740ab8dc4e1ad2017802bb0a264a5. |
| [0cc39ad41e1b424a870c5859c52e0d2916f7d4ec](https://github.com/topfreegames/pitaya/commit/0cc39ad41e1b424a870c5859c52e0d2916f7d4ec) | 2025-03-17 | feature | P2 | unreviewed | Adds a same-server loopback setting; review with fork RPC routing. |
| [c64581111f9f3030622436bb8bac17097b31b986](https://github.com/topfreegames/pitaya/commit/c64581111f9f3030622436bb8bac17097b31b986) | 2025-03-17 | correctness | P2 | unreviewed | Changes cross-platform signal handling; review with App lifecycle ownership. |
| [aaa82e2870feb56f705f159c501ccf0f67543940](https://github.com/topfreegames/pitaya/commit/aaa82e2870feb56f705f159c501ccf0f67543940) | 2025-03-18 | feature | P2 | unreviewed | Adds an upstream server constructor; fork App constructor compatibility not reviewed. |
| [c309ea5afe4bb97c169c2b1d5b664001f0e74aaf](https://github.com/topfreegames/pitaya/commit/c309ea5afe4bb97c169c2b1d5b664001f0e74aaf) | 2025-03-18 | correctness | P2 | unreviewed | Fixes the loopback configuration property and tests; review with its parent feature. |
| [c5cd18732836230952cd3081bb9720087eb33e71](https://github.com/topfreegames/pitaya/commit/c5cd18732836230952cd3081bb9720087eb33e71) | 2025-03-19 | docs | P3 | unreviewed | Documents the upstream loopback property; only relevant if the feature is adopted. |
| [ddc9083a18fa497cb6d25046f85398dcd1a51cd2](https://github.com/topfreegames/pitaya/commit/ddc9083a18fa497cb6d25046f85398dcd1a51cd2) | 2025-03-24 | correctness | P0 | reviewed-pending | required: redesign die-channel close ownership as one fork-native lifecycle batch with race tests. |
| [37fa8dc3ca75b07bd8b35ffd976645ac4e7f30e8](https://github.com/topfreegames/pitaya/commit/37fa8dc3ca75b07bd8b35ffd976645ac4e7f30e8) | 2025-03-24 | correctness | P0 | reviewed-pending | required: separate public notification from the internally owned shutdown channel without widening App races. |
| [13232c624e8b218b6958fef5413d14fb1588433d](https://github.com/topfreegames/pitaya/commit/13232c624e8b218b6958fef5413d14fb1588433d) | 2025-03-24 | maintenance | P3 | covered-by-parents | parents: c64581111f9f3030622436bb8bac17097b31b986 and c5cd18732836230952cd3081bb9720087eb33e71. |
| [cca67c003d09051888bf580e7bf78f0a0f668205](https://github.com/topfreegames/pitaya/commit/cca67c003d09051888bf580e7bf78f0a0f668205) | 2025-04-11 | correctness | P0 | reviewed-pending | required: make repeated termination and shutdown idempotent after channel ownership is redesigned. |
| [9d0bc4aaab5d6da469231266c941b6beae51ff0b](https://github.com/topfreegames/pitaya/commit/9d0bc4aaab5d6da469231266c941b6beae51ff0b) | 2025-04-25 | observability | P1 | blocked | blocked-by: F-18 response-time summary/histogram naming and Prometheus collector ownership decision. |
| [86abbf6f6eca48fd4d79378c9dd6e2808445bbdb](https://github.com/topfreegames/pitaya/commit/86abbf6f6eca48fd4d79378c9dd6e2808445bbdb) | 2025-04-25 | correctness | P1 | blocked | blocked-by: F-18 metrics naming and registration decision; review together with 9d0bc4a. |
| [775658ce08ea920c204e3dd6c10eafb0ac9c841c](https://github.com/topfreegames/pitaya/commit/775658ce08ea920c204e3dd6c10eafb0ac9c841c) | 2025-05-07 | test | P3 | unreviewed | Runs die-channel tests as standalone; fork test isolation semantics differ. |
| [e24f2522bd25c071d0b03c2b48a7ef091cf3f068](https://github.com/topfreegames/pitaya/commit/e24f2522bd25c071d0b03c2b48a7ef091cf3f068) | 2025-05-07 | maintenance | P3 | covered-by-parents | parents: 86abbf6f6eca48fd4d79378c9dd6e2808445bbdb and 775658ce08ea920c204e3dd6c10eafb0ac9c841c. |
| [5c06377bd26c6a16d1bafb7a3d55ffc1b2d78d99](https://github.com/topfreegames/pitaya/commit/5c06377bd26c6a16d1bafb7a3d55ffc1b2d78d99) | 2025-05-26 | correctness | P0 | reviewed-pending | required: propagate unique-session kick failure and prove old registration cannot block a new bind. |
| [209c8deca4945f8ad39ee4659e379b37b2a8f69c](https://github.com/topfreegames/pitaya/commit/209c8deca4945f8ad39ee4659e379b37b2a8f69c) | 2025-06-04 | correctness | P2 | unreviewed | Wraps agent kick error rather than metadata; review with fork error protocol. |
| [5e210c85e903bbbf7f994531c024edb0f7ed12d6](https://github.com/topfreegames/pitaya/commit/5e210c85e903bbbf7f994531c024edb0f7ed12d6) | 2025-07-10 | dependency | P2 | already-equivalent | fork-ref: e275630; verify: root, CLI, and xk6 declare fork module paths and make module-verify rejects upstream identity. |
| [01338f1425d176330c8c21dcafe6b9525d177f46](https://github.com/topfreegames/pitaya/commit/01338f1425d176330c8c21dcafe6b9525d177f46) | 2025-07-10 | dependency | P2 | already-equivalent | fork-ref: dbe1e58 and 0791d29; verify: xk6 imports grafana/sobek and make compile-xk6 succeeds. |
| [b94ba129ecf432105610017a18a6955ad3eb706a](https://github.com/topfreegames/pitaya/commit/b94ba129ecf432105610017a18a6955ad3eb706a) | 2025-07-15 | dependency | P2 | unreviewed | Unifies upstream xk6 versions; compare against current separately versioned module. |
| [41cedb8e6643497edfc49bd8d226c5bc36c1fd6f](https://github.com/topfreegames/pitaya/commit/41cedb8e6643497edfc49bd8d226c5bc36c1fd6f) | 2025-09-09 | observability | P3 | unreviewed | Reduces etcd sync-server log level; not semantically reviewed. |
| [b524a3026e039f80d93e63b14aaba7b8ed0bd222](https://github.com/topfreegames/pitaya/commit/b524a3026e039f80d93e63b14aaba7b8ed0bd222) | 2025-09-09 | correctness | P1 | reviewed-pending | required: recover compacted/future watch revisions with deterministic restart and shutdown race tests. |
| [4f5c74bf4a0a420113695e8ec583ff20dc9be483](https://github.com/topfreegames/pitaya/commit/4f5c74bf4a0a420113695e8ec583ff20dc9be483) | 2025-10-30 | correctness | P1 | reviewed-pending | required: implement StatsD histogram reporting with reporter tests and naming compatibility evidence. |
| [ac4b8a43311a941f7898d0d950b745c98b7c1e54](https://github.com/topfreegames/pitaya/commit/ac4b8a43311a941f7898d0d950b745c98b7c1e54) | 2025-11-21 | feature | P2 | unreviewed | Adds lame-duck readiness behavior; review with fork shutdown and discovery ownership. |
| [d0a71b17466516a75f92b54df869a114170bbd65](https://github.com/topfreegames/pitaya/commit/d0a71b17466516a75f92b54df869a114170bbd65) | 2025-11-21 | security | P0 | reviewed-pending | required: register etcd username/password in Viper defaults and test snapshot/reloadable redaction and authentication. |
| [7343bca923faea14d44e0e0f7d74f1343609d0d4](https://github.com/topfreegames/pitaya/commit/7343bca923faea14d44e0e0f7d74f1343609d0d4) | 2026-01-16 | feature | P2 | unreviewed | Adds readiness API and checks; review with existing fork lifecycle semantics. |
| [955929e76f2dcf8dd049c2c8fcb5a58999f80f5e](https://github.com/topfreegames/pitaya/commit/955929e76f2dcf8dd049c2c8fcb5a58999f80f5e) | 2026-04-30 | correctness | P1 | reviewed-pending | required: capture error status before compression mutates payload and test compressed/uncompressed error frames. |
| [ccf9f673817b14c799e85ad302950f09036d0a80](https://github.com/topfreegames/pitaya/commit/ccf9f673817b14c799e85ad302950f09036d0a80) | 2026-06-09 | observability | P2 | unreviewed | Instruments buffers and worker pools; reconcile with fork metric ownership before adoption. |
| [4ab87bee49567cd6c36d07d5e72a07967770a97a](https://github.com/topfreegames/pitaya/commit/4ab87bee49567cd6c36d07d5e72a07967770a97a) | 2026-06-09 | correctness | P0 | reviewed-pending | required: reject nil NATS request messages without panic and preserve the fork's current nats.go version. |
| [45717a2f25acc3419b9f94ef1f9fbc4c14d13e7c](https://github.com/topfreegames/pitaya/commit/45717a2f25acc3419b9f94ef1f9fbc4c14d13e7c) | 2026-06-18 | correctness | P0 | reviewed-pending | required: eliminate shutdown send-on-closed appDie races as part of the complete die-channel ownership batch. |

## Updating the snapshot

Do not append rows from a floating network response. First fetch and pin a new
`upstream/v2` tip, record the new merge base, regenerate the complete ordered
delta, update the pinned ordered-SHA digest, and run `make docs-lint`. The lint
always verifies that digest. When the pinned base and tip objects are available,
it also compares every ordered ledger SHA with that fixed commit range; it never
derives the snapshot from a floating branch or the fork's current `main`.
