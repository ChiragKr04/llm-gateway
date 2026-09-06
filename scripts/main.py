from openai import OpenAI

c = OpenAI(base_url="http://localhost:8081/v1", api_key="unused")

print(c.chat.completions.create(
    model="mock-small",
    messages=[{"role": "user", "content": "hi"}],
).usage)

usage = None
for chunk in c.chat.completions.create(
    model="mock-small",
    stream=True,
    messages=[{"role": "user", "content": "hi"}],
):
    # The terminal usage chunk carries no choices, so guard the index.
    if chunk.choices:
        print(chunk.choices[0].delta.content or "", end="", flush=True)
    if chunk.usage:
        usage = chunk.usage

print()
print("stream usage:", usage)
