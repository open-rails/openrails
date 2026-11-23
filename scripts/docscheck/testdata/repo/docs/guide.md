# Guide

## First steps

Call `openrails.New`, then `Client.CreateOrder` with `GET /v1/me/orders`.
A refusal is 403 `merchant_unresolved`; staff hold `merchant:catalog:read`.
Fulfil on `order.completed`. See [the client](client.md#methods) and [below](#first-steps).

```go guide:demo-hello
fmt.Println("hello")
fmt.Println("bye")
```

```yaml guide:demo-yaml
slug: demo
name: Demo
```
