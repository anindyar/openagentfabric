# vllm-glm53: the engine for GLM-5.3-Flash on GB10

vLLM's published GLM-5.3-Flash build has no attention backend for the model's sparse MLA on GB10
(SM121), with FP8 or BF16 KV, and aborts at 512K-context shapes. This image adds what it takes to
serve GLM-5.3-Flash NVFP4 across a pair (TP=2) at 512K: the catalog's `pair-tp2` profile runs on it
(engine `vllm-glm53` in `bundle/versions.yaml`).

## Provenance

The image was built on 2026-08-30 and served GLM-5.3-Flash on a GB10 pair from then on. Its build
was recovered from the image itself (layer history and the patch files in its layers); the
[Dockerfile](Dockerfile) is that history, step for step:

| Step | What it changes |
|---|---|
| `patches/sm12x_sparse_mla.py` | Lets SM12x use the SM90 FlashInfer sparse-MLA backend (FA2 on SM12x); FlashInfer's version gate applies to FP8 KV only |
| `patches/nan_debug_hooks.py` | Hooks that log the first non-finite output per module; inactive unless `GLM53_NAN_DEBUG=1` |
| pip pins | FlashInfer 0.6.18.dev20260819 (nightly), NCCL 2.30.7, CUTLASS DSL 4.6.2 |
| `patches/pdl_off_sm12x.py` | Programmatic dependent launch only on SM90/SM100 (races in the KDA state kernels on SM12x) |
| `patches/patch_v7.py` | Indexer top-k buffers start at -1; pool expansion clamped to the pool length |
| `patches/patch_v8_fp8.py` | FlashInfer FA2 MLA with FP8 KV on SM12x: tile capped to fit ~101 KB shared memory |
| `patches/patch_v9_512k.py` | No persistent top-k / cooperative paths on CC 12.x (they need 62 CTAs on a 48-SM GPU), so 512K requests run. Mirrors the sparse-indexer patches of `FujitsuPolycom/glm53-flash-tp2-spark` (Apache-2.0) |

Every patch checks that the code it changes is exactly what it expects and refuses otherwise, so a
rebuild on a different base fails loudly instead of building something untested.

## The released image

The release is the original build, unchanged (image ID
`sha256:29732e5b0c8f4e53417347d7b38f95d5a547e2ce2e63f51f634aebdc0fdadc20`), not a rebuild: the
base tag `vllm/vllm-openai:glm53-flash-arm64-cu130` has moved since it was built. Licences: vLLM
(Apache-2.0) and the NVIDIA CUDA Toolkit EULA, as for the upstream image.

## Rebuilding

Only to change a patch. Pin the base by digest, build on a GB10 (arm64), and run the acceptance
test on a pair before pinning the result:

```sh
docker build -t vllm-glm53:dev deploy/engines/vllm-glm53
```
