# Go Load Balancer

[![Go CI](https://github.com/louisphilipmarcoux/go-load-balancer/actions/workflows/go.yml/badge.svg)](https://github.com/louisphilipmarcoux/go-load-balancer/actions)

A high-performance, production-ready load balancer built from scratch in Go.

This project is an implementation of the "Build Your Own Load Balancer" challenge.

## Features (In Progress)

- [x] Stage 1: TCP Proxy
- [x] Stage 2: Concurrent Connections
- [ ] Stage 3: Backend Pool

## How to Run

1. **Start a backend server:**

    ```bash
    go run ./backend -port 9001 Server-1
    ```

2. **Start the load balancer:**

    ```bash
    go run .
    ```

3. **Test it:**

    ```bash
    curl http://localhost:8080/
    # Output: Hello from backend server: Server-1
    ```
