# vllm-glm53: the vLLM build that serves GLM-5.3-Flash on GB10 (SM121).
#
# Recovered on 2026-10-28 from the image that served it (glm53:v9, image ID
# sha256:29732e5b0c8f4e53417347d7b38f95d5a547e2ce2e63f51f634aebdc0fdadc20):
# every step below is that image's build history after its base, verbatim.
# The released image is that exact build (see README.md); rebuilding from
# this file is for changing a patch, and needs testing on a pair first.
#
# The base is vLLM's day-0 GLM-5.3-Flash image. Its tag has moved since
# (the build used a 32-layer version; the tag now has 37 layers), so pin the
# base you test with by digest.
FROM vllm/vllm-openai:glm53-flash-arm64-cu130

# Sparse MLA on SM12x: allow the SM90 FlashInfer backend (FA2 there) with
# BF16 KV; vLLM's published build has no attention backend for it on GB10.
COPY patches/sm12x_sparse_mla.py /tmp/p.py
RUN python3 /tmp/p.py && rm /tmp/p.py

# Debug hooks for non-finite outputs; inactive unless GLM53_NAN_DEBUG=1.
COPY patches/nan_debug_hooks.py /tmp/p.py
RUN python3 /tmp/p.py && rm /tmp/p.py

RUN pip install -q --pre       flashinfer-python==0.6.18.dev20260819       flashinfer-cubin==0.6.18.dev20260819       --extra-index-url https://flashinfer.ai/whl/nightly/     && pip uninstall -q -y flashinfer-jit-cache     && python3 -c "import flashinfer; print('flashinfer', flashinfer.__version__)"
RUN pip install -q nvidia-nccl-cu13==2.30.7 && pip show nvidia-nccl-cu13 | grep -q "Version: 2.30.7" && echo NCCL-RESTORED
RUN pip install -q nvidia-cutlass-dsl==4.6.2 && pip show nvidia-cutlass-dsl | grep -q "Version: 4.6.2" && echo DSL-RESTORED

# Programmatic dependent launch off on SM12x (races in the KDA state kernels).
COPY patches/pdl_off_sm12x.py /tmp/p.py
RUN python3 /tmp/p.py && rm /tmp/p.py

COPY patches/patch_v7.py /tmp/patch_v7.py
RUN python3 /tmp/patch_v7.py && rm /tmp/patch_v7.py
COPY patches/patch_v8_fp8.py /tmp/patch_v8_fp8.py
RUN python3 /tmp/patch_v8_fp8.py && rm /tmp/patch_v8_fp8.py
COPY patches/patch_v9_512k.py /tmp/patch_v9_512k.py
RUN python3 /tmp/patch_v9_512k.py && rm /tmp/patch_v9_512k.py
