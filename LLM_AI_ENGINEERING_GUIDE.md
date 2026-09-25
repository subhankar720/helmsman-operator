# LLM Hosting & AI Engineering for DevOps Engineers

> **Audience:** DevOps/SRE engineers transitioning into AI engineering.
> **Goal:** Explain why LLM hosting works the way it does, how to serve models reliably at scale, and how to extend your existing Kubernetes/GitOps platform for AI workloads. Includes a hands-on study plan and implementation guide.

---

## Table of Contents

1. [Why This Matters for DevOps](#1-why-this-matters-for-devops)
2. [LLM & Transformer Fundamentals](#2-llm--transformer-fundamentals)
3. [Model Lifecycle & Formats](#3-model-lifecycle--formats)
4. [Serving Patterns](#4-serving-patterns)
5. [Inference Optimization](#5-inference-optimization)
6. [RAG & Knowledge Systems](#6-rag--knowledge-systems)
7. [AI Agents](#7-ai-agents)
8. [Prompt Engineering & Evaluation](#8-prompt-engineering--evaluation)
9. [Platform Concerns: GPU, Memory, Scheduling](#9-platform-concerns-gpu-memory-scheduling)
10. [Observability for AI](#10-observability-for-ai)
11. [Security & Governance](#11-security--governance)
12. [Cost Management](#12-cost-management)
13. [Study Plan](#13-study-plan)
14. [Implementation Roadmap](#14-implementation-roadmap)

---

## 1. Why This Matters for DevOps

### 1.1 The Shift

| Traditional App | AI App |
|-----------------|--------|
| Stateless microservices | Stateful model servers |
| CPU-bound | GPU/TPU-bound |
| Predictable latency | Variable inference latency |
| Deterministic output | Stochastic output |
| Versioned containers | Versioned model artifacts |
| Logs + metrics | Logs + metrics + embeddings + traces |

As a DevOps engineer, you already own:
- **Infrastructure provisioning** → now includes GPU nodes
- **CI/CD** → now includes model pipelines
- **Observability** → now includes LLM-specific metrics
- **Security** → now includes prompt injection, data exfiltration
- **Scheduling** → now includes memory-aware model placement

### 1.2 What’s Actually New

The *platform* concerns are familiar:
- Packaging → container images
- Deployment → Kubernetes manifests/operators
- Networking → Services, Ingress, NetworkPolicy
- Secrets → Vault/ESO
- Observability → Prometheus, logs, traces

The *new* concerns:
- Model weights (GBs to TBs)
- GPU memory management
- Batching and queuing for inference
- Prompt/response logging and evaluation
- Vector databases for RAG
- Model versioning and rollback

### 1.3 Your Superpowers as a DevOps Engineer

- **You know Kubernetes deeply** — model serving runs on K8s
- **You know GitOps** — model deployments can be GitOps-driven
- **You know observability** — LLM systems need the same rigor
- **You know security** — LLMs have new attack surfaces, but same principles
- **You know automation** — ML pipelines are just CI/CD for models

---

## 2. LLM & Transformer Fundamentals

### 2.1 What is a Transformer?

A transformer is a neural network architecture that processes sequences (text, images, audio) using **self-attention**.

```
Input: "The cat sat on the mat"
  │
  ▼
Tokenization: ["The", "cat", "sat", "on", "the", "mat"]
  │
  ▼
Embedding: [[0.1, -0.3, ...], [0.5, 0.2, ...], ...]
  │
  ▼
Self-Attention: Each token attends to all other tokens
  │
  ▼
Feed-Forward + Layer Norm
  │
  ▼
Output Logits: Probabilities for next token
  │
  ▼
Sampling: "The cat sat on the mat because it was tired"
```

### 2.2 Key Concepts

| Concept | Meaning | DevOps Analogy |
|---------|---------|----------------|
| **Token** | Piece of text (word/subword) | Request unit |
| **Context Window** | Max tokens model can process | Max request size |
| **Parameters** | Model weights count | Binary size |
| **Inference** | Running model to generate output | Serving a request |
| **Training** | Adjusting weights from data | Building a new image |
| **Fine-tuning** | Updating weights on specific data | Patching a container |
| **Temperature** | Randomness in generation | Chaos engineering knob |
| **Top-p / Top-k** | Sampling strategies | Load balancing algorithms |

### 2.3 Model Sizes

| Model | Parameters | VRAM Needed (FP16) | Use Case |
|-------|------------|-------------------|----------|
| Llama 2 7B | 7B | ~14GB | Development, edge |
| Llama 2 13B | 13B | ~26GB | Small production |
| Llama 2 70B | 70B | ~140GB | Large production |
| Mixtral 8x7B | 47B active | ~90GB | Production, MoE |
| GPT-4 | ~1.7T (estimated) | N/A (API) | Largest tasks |

### 2.4 The Inference Loop

```
Prompt: "Explain Kubernetes"
  │
  ▼
Tokenize: [101, 456, 789, ...]
  │
  ▼
Forward Pass (GPU):
  - Load weights into VRAM
  - Compute attention layers
  - Generate logits for next token
  │
  ▼
Sample next token: "in"
  │
  ▼
Repeat until stop token or max length
  │
  ▼
Decode: "in simple terms, Kubernetes is..."
```

---

## 3. Model Lifecycle & Formats

### 3.1 Model Sources

- **Hugging Face Hub** — `meta-llama/Llama-2-7b-chat-hf`
- **Model Zoo** — ONNX Model Zoo, TensorFlow Hub
- **Custom training** — Your own models
- **Model Registry** — MLflow, Hugging Face Registry, S3/GCS

### 3.2 Model Formats

| Format | Use Case | Tooling |
|--------|----------|---------|
| **PyTorch/Safetensors** | Training, raw weights | PyTorch, Hugging Face |
| **GGUF** | CPU/edge inference | llama.cpp, Ollama |
| **ONNX** | Cross-platform, CPU | ONNX Runtime |
| **TensorRT** | NVIDIA GPU optimization | TensorRT-LLM |
| **vLLM** | High-throughput serving | vLLM |
| **TGI** | Hugging Face serving | Text Generation Inference |

### 3.3 Versioning Models

```bash
# Models are artifacts, just like containers
mlflow models log-model \
  --model-name llama-2-7b-chat \
  --artifact-path ./model \
  --tags git_commit=abc123,dataset=custom-v2

# Or with Git LFS
git lfs track "*.bin"
git add model/llama-2-7b-chat.bin
git commit -m "feat: update model to v1.2"
git push
```

### 3.4 Model Pipeline (CI/CD for Models)

```yaml
# .github/workflows/model-pipeline.yml
name: Model Pipeline

on:
  push:
    paths:
      - 'models/**'
      - 'training/**'

jobs:
  evaluate:
    runs-on: [self-hosted, gpu]
    steps:
      - uses: actions/checkout@v4
      - name: Run evaluation
        run: python scripts/evaluate.py --model models/llama-2-7b-chat.bin
      - name: Upload metrics
        run: python scripts/log_metrics.py

  promote:
    needs: evaluate
    if: github.ref == 'refs/heads/main'
    runs-on: ubuntu-latest
    steps:
      - name: Promote to staging
        run: |
          kubectl apply -f serving/staging/inference-service.yaml
      - name: Run load test
        run: python scripts/load_test.py --endpoint http://staging-llm:8080
      - name: Promote to production
        run: |
          kubectl apply -f serving/prod/inference-service.yaml
```

---

## 4. Serving Patterns

### 4.1 Single Model, Single Replica

```yaml
# Simplest pattern: one model, one pod
apiVersion: apps/v1
kind: Deployment
metadata:
  name: llama-2-7b
spec:
  replicas: 1
  selector:
    matchLabels:
      app: llama-2-7b
  template:
    metadata:
      labels:
        app: llama-2-7b
    spec:
      containers:
      - name: vllm
        image: vllm/vllm-openai:latest
        args:
        - --model
        - meta-llama/Llama-2-7b-chat-hf
        - --tensor-parallel-size
        - "1"
        ports:
        - containerPort: 8000
        resources:
          limits:
            nvidia.com/gpu: 1
            memory: 32Gi
```

### 4.2 Tensor Parallelism

Split one model across multiple GPUs:

```bash
# 2 GPUs, model split across both
vllm serve meta-llama/Llama-2-70b-chat-hf \
  --tensor-parallel-size 2
```

```yaml
# Kubernetes
resources:
  limits:
    nvidia.com/gpu: 2  # Two GPUs for one model instance
```

### 4.3 Pipeline Parallelism

For models too large for one node:

```
┌─────────────┐   ┌─────────────┐   ┌─────────────┐
│   Node 1    │   │   Node 2    │   │   Node 3    │
│  Layers 1-20│ → │ Layers 21-40│ → │ Layers 41-60│
│  GPU 0      │   │  GPU 0      │   │  GPU 0      │
└─────────────┘   └─────────────┘   └─────────────┘
```

### 4.4 Batching

Combine multiple requests into one forward pass:

```
Request 1: "What is K8s?"
Request 2: "What is Docker?"
Request 3: "What is Helm?"
         │
         ▼
    [Batch Form]
         │
         ▼
    [Single GPU Forward Pass]
         │
         ▼
Response 1: "Kubernetes is..."
Response 2: "Docker is..."
Response 3: "Helm is..."
```

### 4.5 Serving with KServe

```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: llama-2-7b
spec:
  predictor:
    model:
      modelFormat:
        name: vLLM
      storageUri: "s3://models/llama-2-7b-chat"
      resources:
        limits:
          memory: "32Gi"
          cpu: "8"
          nvidia.com/gpu: "1"
  autoscaling:
    minReplicas: 1
    maxReplicas: 10
    scaleDown:
      windowSeconds: 300
    scaleUp:
      windowSeconds: 60
```

### 4.6 Model Loading Strategies

| Strategy | Description | Use Case |
|----------|-------------|----------|
| **Eager** | Load full model at startup | Low latency, high throughput |
| **Lazy** | Load layers on demand | Memory constrained |
| **Streaming** | Load from storage progressively | Large models, slow storage |
| **Shared** | Multiple pods share model via shared memory | High density, same model |

---

## 5. Inference Optimization

### 5.1 Quantization

Reduce precision to fit more model in memory:

| Precision | VRAM | Quality Loss | Use Case |
|-----------|------|--------------|----------|
| FP16 | 2x model size | None | Baseline |
| INT8 | ~1.5x | Minimal | Good tradeoff |
| INT4 | ~0.75x | Noticeable | Edge, development |
| GGUF Q4_K_M | ~0.5x | Moderate | CPU, llama.cpp |

### 5.2 KV Cache

Cache key-value pairs from previous tokens:

```
Token 1: "The"      → Compute attention, store K,V
Token 2: " cat"     → Reuse K,V for "The", compute for " cat"
Token 3: " sat"     → Reuse K,V for "The cat", compute for " sat"
...
```

This is why longer contexts are expensive — KV cache grows with sequence length.

### 5.3 Continuous Batching

Instead of waiting for all requests in a batch to finish:
- Add new requests as others complete
- Remove completed requests early

### 5.4 Speculative Decoding

```
Draft Model (small) → generates 4 tokens quickly
Verifier Model (large) → verifies all 4 in one pass
```

Can give 2-3x speedup with same quality.

### 5.5 PagedAttention

vLLM’s key innovation: manage KV cache in pages, like virtual memory.

```
┌─────┬─────┬─────┬─────┬─────┐
│ Pg 1│ Pg 2│ Pg 3│ Pg 4│ Pg 5│
├─────┼─────┼─────┼─────┼─────┤
│Req A│Req B│     │Req D│     │
│     │     │     │     │     │
└─────┴─────┴─────┴─────┴─────┘
```

Memory is allocated in fixed-size pages, reducing fragmentation.

---

## 6. RAG & Knowledge Systems

### 6.1 Why RAG?

LLMs have knowledge cutoffs and can hallucinate. RAG (Retrieval-Augmented Generation) grounds responses in your data.

```
User Query: "What is our refund policy?"
  │
  ▼
┌─────────────────┐
│ Embedding Model │ → [0.1, -0.3, 0.5, ...]
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│  Vector DB      │ ← Find similar chunks
│  (Pinecone,     │
│   Milvus, etc.) │
└────────┬────────┘
         │
         ▼
Top-K Chunks: [
  "Refunds are processed within 5-7 business days...",
  "To request a refund, go to Settings → Billing..."
]
         │
         ▼
┌─────────────────┐
│   LLM Prompt    │
│                 │
│ Context:        │
│ 1. Refunds are  │
│    processed... │
│ 2. To request...│
│                 │
│ Question:       │
│ What is our     │
│ refund policy?  │
│                 │
│ Answer:         │
└────────┬────────┘
         │
         ▼
"According to our policy, refunds are processed within
5-7 business days. To request one, go to Settings → Billing."
```

### 6.2 Embeddings

Convert text to vectors:

```python
from sentence_transformers import SentenceTransformer

model = SentenceTransformer('all-MiniLM-L6-v2')
embeddings = model.encode([
    "What is the refund policy?",
    "How do I return an item?",
    "Our refund policy states..."
])
# embeddings.shape = (3, 384)
```

### 6.3 Vector Databases

| Option | Deployment | Scale | Use Case |
|--------|-----------|-------|----------|
| **Pinecone** | Managed | Large | Production, low ops |
| **Weaviate** | Self-hosted | Medium | Open-source, Kubernetes |
| **Milvus** | Self-hosted | Large | Enterprise, Kubernetes |
| **Qdrant** | Self-hosted | Medium | Rust-based, fast |
| **Chroma** | Embedded | Small | Development, prototyping |
| **PostgreSQL + pgvector** | Existing DB | Medium | Simple, co-located |

### 6.4 Chunking Strategies

```python
def chunk_text(text, chunk_size=512, overlap=50):
    """Split text into overlapping chunks."""
    tokens = tokenize(text)
    chunks = []
    for i in range(0, len(tokens), chunk_size - overlap):
        chunk = tokens[i:i + chunk_size]
        chunks.append(chunk)
    return chunks
```

| Strategy | Description | Best For |
|----------|-------------|----------|
| **Fixed-size** | Split by token count | General purpose |
| **Sentence** | Split on sentences | Conversational |
| **Paragraph** | Split on paragraphs | Documents |
| **Semantic** | Split on topic changes | Technical docs |
| **Recursive** | Try markdown headers, then sentences | Markdown files |

### 6.5 RAG Evaluation

```python
# Metrics
metrics = {
    "retrieval_precision": "Are retrieved chunks relevant?",
    "retrieval_recall": "Are all relevant chunks retrieved?",
    "answer_faithfulness": "Is the answer grounded in context?",
    "answer_relevance": "Does the answer address the question?"
}
```

---

## 7. AI Agents

### 7.1 What is an Agent?

An agent is a system that:
1. Receives a goal
2. Reasons about steps to achieve it
3. Uses tools to execute steps
4. Observes results
5. Adapts and repeats

```
User: "Research competitors and summarize pricing"
  │
  ▼
┌─────────────────────────────────────────┐
│             Agent Loop                   │
│                                          │
│  1. Plan: Search web, visit sites,       │
│            extract pricing, summarize    │
│  2. Act:    web_search("competitors")     │
│  3. Observe: Found 3 competitors         │
│  4. Act:    visit_site(competitor_1)     │
│  5. Observe: Pricing: $29/mo            │
│  6. Act:    visit_site(competitor_2)     │
│  7. Observe: Pricing: $39/mo            │
│  8. Act:    visit_site(competitor_3)     │
│  9. Observe: Pricing: $19/mo            │
│  10. Plan: All data collected, summarize │
│  11. Act:    generate_summary()          │
│  12. Return: Summary to user             │
└─────────────────────────────────────────┘
```

### 7.2 Agent Components

| Component | Role | Example |
|-----------|------|---------|
| **LLM** | Reasoning engine | Llama 2, GPT-4 |
| **Memory** | Conversation history | Redis, PostgreSQL |
| **Tools** | External actions | Web search, DB query, API call |
| **Planner** | Break down goals | ReAct, Chain of Thought |
| **Executor** | Run tools | Python functions, API clients |
| **Observer** | Parse results | Output parsers, validators |

### 7.3 Agent Patterns

**ReAct (Reason + Act)**
```
Thought: I need to find the weather
Action: weather_api("Seattle")
Observation: 72°F, sunny
Thought: The weather is 72°F and sunny
Answer: It's 72°F and sunny in Seattle
```

**Plan-and-Solve**
```
Plan:
1. Search for competitors
2. Extract pricing
3. Create comparison table
4. Write summary

Solve each step sequentially
```

**Multi-Agent**
```
Researcher Agent → gathers information
Writer Agent → drafts content
Reviewer Agent → checks quality
Coordinator → manages workflow
```

### 7.4 Tool Use

```python
tools = [
    {
        "name": "web_search",
        "description": "Search the web for information",
        "parameters": {
            "query": {"type": "string", "description": "Search query"}
        }
    },
    {
        "name": "database_query",
        "description": "Query the company database",
        "parameters": {
            "sql": {"type": "string", "description": "SQL query"}
        }
    }
]

# LLM decides which tool to use
response = llm.chat(messages, tools=tools)
# response = {
#   "tool_calls": [{"name": "web_search", "arguments": {"query": "competitors"}}]
# }
```

### 7.5 Kubernetes for Agents

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: research-agent
spec:
  replicas: 5
  selector:
    matchLabels:
      app: research-agent
  template:
    metadata:
      labels:
        app: research-agent
    spec:
      containers:
      - name: agent
        image: your-org/research-agent:latest
        env:
        - name: LLM_ENDPOINT
          value: http://vllm-service:8000
        - name: REDIS_URL
          value: redis://memory-store:6379
        resources:
          requests:
            cpu: "500m"
            memory: "1Gi"
          limits:
            cpu: "2000m"
            memory: "4Gi"
```

---

## 8. Prompt Engineering & Evaluation

### 8.1 Prompt Structure

```
System Prompt: "You are a helpful assistant that answers questions about Kubernetes."
  │
  ▼
User Prompt: "What is a Pod?"
  │
  ▼
Assistant Response: "A Pod is the smallest deployable unit in Kubernetes..."
```

### 8.2 Prompt Techniques

| Technique | Description | Example |
|-----------|-------------|---------|
| **Zero-shot** | Direct question | "What is 2+2?" |
| **Few-shot** | Provide examples | "Q: 2+2? A: 4. Q: 3+3? A: 6. Q: 4+4? A:" |
| **Chain-of-Thought** | Ask for reasoning | "Think step by step: If a train leaves..." |
| **ReAct** | Interleave reasoning and actions | "Thought: I need to search. Action: web_search()" |
| **Constitutional AI** | Self-critique and revise | "Critique your answer, then revise" |

### 8.3 Prompt Templates

```python
# templates/kubernetes_expert.yaml
system: |
  You are a Kubernetes expert assistant.
  Always provide accurate, actionable information.
  Cite official docs when possible.

user: |
  Question: {question}
  Context: {context}
  
  Answer:
```

### 8.4 Evaluation

```python
# metrics/evaluation.py
def evaluate_model(model, test_cases):
    metrics = {
        "accuracy": 0,
        "latency_p50": 0,
        "latency_p99": 0,
        "token_throughput": 0
    }
    
    for case in test_cases:
        start = time.time()
        response = model.generate(case["prompt"])
        latency = time.time() - start
        
        metrics["accuracy"] += score_response(response, case["expected"])
        metrics["latency_p50"].append(latency)
        metrics["token_throughput"] += len(response) / latency
    
    return metrics
```

### 8.5 A/B Testing Prompts

```yaml
# Experiment 1: Test two prompts
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: prompt-experiment-1
spec:
  source:
    repoURL: https://github.com/your-org/ai-configs.git
    targetRevision: main
    path: experiments/prompt-v1
  # Canary: 10% traffic to v1, 90% to v2
```

---

## 9. Platform Concerns: GPU, Memory, Scheduling

### 9.1 GPU in Kubernetes

```bash
# Install NVIDIA device plugin
kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/kubernetes-engine-samples/main/ai-ml/gpu-device-plugin/gpu-device-plugin.yaml

# Verify GPUs are visible
kubectl get nodes -o yaml | grep -A 10 "nvidia.com/gpu"
```

### 9.2 GPU Resource Requests

```yaml
resources:
  requests:
    nvidia.com/gpu: 1
  limits:
    nvidia.com/gpu: 1
    memory: 32Gi
```

### 9.3 GPU Sharing (MPS or MIG)

```yaml
# NVIDIA MIG (Multi-Instance GPU)
# Split one A100 into 7 instances
resources:
  limits:
    nvidia.com/mig-1g.10gb: 1
```

### 9.4 Memory-Aware Scheduling

```yaml
# For large models that need specific memory
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
      - matchExpressions:
        - key: node.kubernetes.io/memory
          operator: Gt
          values:
          - "200Gi"
```

### 9.5 Model Caching

```yaml
# Use emptyDir for model cache to avoid re-downloading
volumes:
- name: model-cache
  emptyDir:
    sizeLimit: 50Gi
volumeMounts:
- name: model-cache
  mountPath: /root/.cache/huggingface
```

---

## 10. Observability for AI

### 10.1 What to Measure

| Metric | Why | Tool |
|--------|-----|------|
| **Requests/sec** | Throughput | Prometheus |
| **Latency (p50, p99)** | User experience | Prometheus |
| **Tokens/sec** | Model efficiency | Custom exporter |
| **GPU utilization** | Resource usage | DCGM exporter |
| **Queue depth** | Backpressure | Custom |
| **Error rate** | Reliability | Prometheus |
| **Cost per request** | Business metric | Custom |
| **Hallucination rate** | Quality | Custom |

### 10.2 Logging Prompts and Responses

```yaml
# Fluent Bit config to capture LLM interactions
[INPUT]
    Name              tail
    Path              /var/log/llm/requests.log
    Parser            json
    Tag               llm.requests

[OUTPUT]
    Name            elasticsearch
    Host            ${ES_HOST}
    Port            ${ES_PORT}
    Index           llm-logs
```

### 10.3 Tracing

```python
# OpenTelemetry for LLM calls
from opentelemetry import trace
from opentelemetry.instrumentation.openai import OpenAIInstrumentor

tracer = trace.get_tracer(__name__)

with tracer.start_as_current_span("llm.completion") as span:
    response = openai.ChatCompletion.create(
        model="gpt-4",
        messages=messages
    )
    span.set_attribute("llm.model", "gpt-4")
    span.set_attribute("llm.tokens_prompt", response.usage.prompt_tokens)
    span.set_attribute("llm.tokens_completion", response.usage.completion_tokens)
    span.set_attribute("llm.latency_ms", latency)
```

### 10.4 Evaluation in Production

```yaml
# Sidecar that evaluates responses
containers:
- name: app
  image: your-app:latest
- name: eval-agent
  image: your-org/eval-agent:latest
  env:
  - name: EVAL_MODEL
    value: gpt-4
  - name: EVAL_PROMPT
    value: "Rate this response 1-10 for accuracy"
```

---

## 11. Security & Governance

### 11.1 Threat Model

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   User      │────▶│  Gateway    │────▶│  LLM API   │
│             │     │             │     │             │
│ (Attacker?) │     │ (Auth?)     │     │ (Prompt     │
│             │     │             │     │  Injection?)│
└─────────────┘     └─────────────┘     └─────────────┘
```

### 11.2 Prompt Injection Defense

```python
def sanitize_prompt(user_input):
    """Basic prompt injection defense."""
    blocked_patterns = [
        "ignore previous instructions",
        "you are now",
        "pretend you are",
        "act as if"
    ]
    
    for pattern in blocked_patterns:
        if pattern in user_input.lower():
            raise ValueError("Potential prompt injection detected")
    
    return user_input
```

### 11.3 Data Exfiltration Prevention

```yaml
# NetworkPolicy: block LLM pods from accessing internal services
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: llm-egress-deny
spec:
  podSelector:
    matchLabels:
      app: llm-inference
  egress:
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: external
    ports:
    - port: 443
      protocol: TCP
```

### 11.4 Model Access Control

```yaml
# RBAC for model access
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ai-team
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: ai-models
rules:
- apiGroups: ["serving.kserve.io"]
  resources: ["inferenceservices"]
  verbs: ["get", "list"]
  resourceNames: ["llama-2-7b", "mixtral-8x7b"]
```

---

## 12. Cost Management

### 12.1 Cost Breakdown

| Component | Cost Factor | Optimization |
|-----------|-------------|--------------|
| **GPUs** | $2-10/hr per GPU | Scale to zero, spot instances |
| **Model storage** | S3/GCS egress | Cache aggressively |
| **Inference** | Tokens generated | Batch, quantize |
| **Embeddings** | Vector DB size | Reduce dimensions, prune |

### 12.2 Scaling Strategies

```yaml
# Scale to zero when idle
autoscaling:
  minReplicas: 0
  maxReplicas: 10
  scaleDown:
    windowSeconds: 300  # 5 min idle before scale down
```

### 12.3 Spot Instances

```yaml
# Use spot/preemptible VMs for inference
nodeSelector:
  cloud.google.com/gke-spot: "true"
# Or
tolerations:
- key: "spot"
  operator: "Equal"
  value: "true"
  effect: "NoSchedule"
```

### 12.4 Cost Monitoring

```bash
# Kubecost
kubectl apply -f https://raw.githubusercontent.com/kubecost/cost-analyzer-helm-chart/develop/kubecost.yaml

# View AI workload costs
kubectl cost namespace ai-agents --context kind-helmsman-hub
```

---

## 13. Study Plan

### Phase 1: Foundations (2-3 weeks)

| Week | Topics | Lab |
|------|--------|-----|
| 1 | Transformer architecture, attention, tokens | Implement a tiny transformer in PyTorch |
| 2 | Model formats (GGUF, ONNX, TensorRT) | Convert a model to GGUF, run with llama.cpp |
| 3 | Inference basics, temperature, top-p | Run Llama 2 7B locally, experiment with parameters |

### Phase 2: Serving (2-3 weeks)

| Week | Topics | Lab |
|------|--------|-----|
| 4 | vLLM, TGI, Ollama | Deploy Llama 2 7B with vLLM on Kubernetes |
| 5 | Batching, quantization, KV cache | Compare FP16 vs INT4, measure throughput |
| 6 | KServe, Knative, autoscaling | Deploy with KServe, test scale-to-zero |

### Phase 3: RAG (2-3 weeks)

| Week | Topics | Lab |
|------|--------|-----|
| 7 | Embeddings, vector DBs | Set up Chroma, embed a document corpus |
| 8 | Chunking strategies, retrieval | Build a RAG pipeline with LangChain |
| 9 | RAG evaluation | Measure retrieval precision/recall |

### Phase 4: Agents (2-3 weeks)

| Week | Topics | Lab |
|------|--------|-----|
| 10 | ReAct, tool use, planning | Build a web-search agent with LangChain |
| 11 | Multi-agent, orchestration | Build a research + writer agent system |
| 12 | Production agents | Containerize, deploy, observe |

### Phase 5: Production Platform (2-3 weeks)

| Week | Topics | Lab |
|------|--------|-----|
| 13 | GPU scheduling, memory management | Set up GPU node pools, test scheduling |
| 14 | Observability (metrics, logs, traces) | Instrument LLM calls with OTel |
| 15 | Security, cost, CI/CD | Add prompt injection defense, cost dashboard |

---

## 14. Implementation Roadmap

### 14.1 Phase 1: Local Development

```bash
# 1. Run a model locally
ollama run llama2:7b

# 2. Build a simple API
pip install fastapi uvicorn
```

```python
# app.py
from fastapi import FastAPI
from vllm import LLM, SamplingParams

app = FastAPI()
llm = LLM(model="meta-llama/Llama-2-7b-chat-hf")

@app.post("/generate")
def generate(prompt: str):
    params = SamplingParams(temperature=0.7, max_tokens=100)
    outputs = llm.generate([prompt], params)
    return {"response": outputs[0].outputs[0].text}
```

```bash
uvicorn app:app --host 0.0.0.0 --port 8080
```

### 14.2 Phase 2: Kubernetes Deployment

```bash
# 1. Build container
docker build -t llm-api:dev .

# 2. Load into Kind
kind load docker-image llm-api:dev --name helmsman-onprem

# 3. Deploy
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: llm-api
spec:
  replicas: 1
  selector:
    matchLabels:
      app: llm-api
  template:
    metadata:
      labels:
        app: llm-api
    spec:
      containers:
      - name: api
        image: llm-api:dev
        imagePullPolicy: Never
        ports:
        - containerPort: 8080
        resources:
          limits:
            nvidia.com/gpu: 1
            memory: 32Gi
EOF
```

### 14.3 Phase 3: RAG Pipeline

```bash
# 1. Set up vector DB
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: chroma
spec:
  replicas: 1
  selector:
    matchLabels:
      app: chroma
  template:
    metadata:
      labels:
        app: chroma
    spec:
      containers:
      - name: chroma
        image: chromadb/chroma:latest
        ports:
        - containerPort: 8000
        resources:
          limits:
            memory: 8Gi
EOF
```

```python
# rag_pipeline.py
import chromadb
from sentence_transformers import SentenceTransformer

# Initialize
client = chromadb.Client()
collection = client.get_or_create_collection("docs")
embedder = SentenceTransformer('all-MiniLM-L6-v2')

def ingest_document(text):
    chunks = chunk_text(text)
    embeddings = embedder.encode(chunks)
    collection.add(
        documents=chunks,
        embeddings=embeddings,
        ids=[f"chunk_{i}" for i in range(len(chunks))]
    )

def query(question):
    query_embedding = embedder.encode([question])
    results = collection.query(
        query_embeddings=query_embedding,
        n_results=3
    )
    context = "\n".join(results['documents'][0])
    return llm.generate(f"Context: {context}\nQuestion: {question}")
```

### 14.4 Phase 4: Agent System

```python
# agent.py
from langchain.agents import initialize_agent, Tool
from langchain.llms import VLLMOpenAI

llm = VLLMOpenAI(
    openai_api_key="EMPTY",
    openai_api_base="http://vllm-service:8000/v1",
    model_name="meta-llama/Llama-2-7b-chat-hf"
)

tools = [
    Tool(
        name="Web Search",
        func=web_search,
        description="Search the web for information"
    ),
    Tool(
        name="Database",
        func=db_query,
        description="Query the company database"
    )
]

agent = initialize_agent(tools, llm, agent="zero-shot-react-description")
result = agent.run("Research competitors and summarize pricing")
```

### 14.5 Phase 5: Production Platform

```yaml
# Complete production setup
# 1. GPU node pool
# 2. KServe for model serving
# 3. Prometheus + Grafana for metrics
# 4. OpenSearch for logs
# 5. Kyverno for policy enforcement
# 6. ArgoCD for GitOps
# 7. Vault for secrets
# 8. Cost monitoring with Kubecost
```

---

## Appendix A: Glossary

| Term | Definition |
|------|------------|
| **Token** | Basic unit of text for LLMs (word/subword) |
| **Context Window** | Max number of tokens an LLM can process at once |
| **Inference** | Running a model to generate output |
| **Fine-tuning** | Updating model weights on domain-specific data |
| **RAG** | Retrieval-Augmented Generation |
| **Embedding** | Dense vector representation of text |
| **Vector Database** | Database optimized for similarity search |
| **Agent** | System that uses LLM + tools to achieve goals |
| **Prompt** | Input text to an LLM |
| **Temperature** | Controls randomness in generation |
| **Top-p / Top-k** | Sampling strategies for token selection |
| **KV Cache** | Cached key-value pairs from previous tokens |
| **Quantization** | Reducing model precision to save memory |
| **Tensor Parallelism** | Splitting model across multiple GPUs |
| **Pipeline Parallelism** | Splitting model layers across nodes |

## Appendix B: Recommended Resources

### Courses
- **Fast.ai** — Practical Deep Learning for Coders
- **CS224n** — Stanford NLP course
- **LLM University** — Cohere’s free course

### Books
- *Building LLM Powered Applications* — Valentina Alto
- *Designing Data-Intensive Applications* — Martin Kleppmann
- *Kubernetes in Action* — Marko Lukša

### Tools to Explore
- **LangChain** — Agent framework
- **LlamaIndex** — RAG framework
- **vLLM** — High-throughput serving
- **KServe** — Enterprise model serving
- **MLflow** — Model registry
- **Weights & Biases** — Experiment tracking

### Communities
- **LangChain Discord**
- **KServe Slack**
- **r/LocalLLaMA** — Local model serving
- **Hugging Face Forums**

---

*This guide is opinionated toward production Kubernetes platforms. It assumes you already know Kubernetes, GitOps, and CI/CD — and want to add LLM/agent capabilities to that foundation.*

