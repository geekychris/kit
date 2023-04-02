# Status

A process can be in one of the following states:

```mermaid
flowchart LR
    waiting --> starting
    starting -->|no readiness| running
    starting -->|ready| ready
    starting -->|not ready| running
    starting -->|error| error
    running -->|exit code >0| error
    running -->|exit code 0| success
    ready -->|exit code >0| error
    ready -->|exit code 0| success
    error -->|backoff| starting
    success -->|backoff| starting

```