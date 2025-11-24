# GitHub Copilot Instructions for Backuparr

## Project Overview
Backuparr is a Scala 3 application for creating and managing backups of *arr applications (Sonarr, Radarr, Lidarr, etc.) with S3 storage support.

## Technology Stack
- **Language**: Scala 3
- **Effect System**: Cats Effect 3.x
- **Programming Paradigm**: Pure Functional Programming

## Core Principles

### 1. Pure Functional Programming
- All code must follow pure FP principles
- Use immutable data structures exclusively
- Avoid side effects; isolate them in IO contexts
- Prefer algebraic data types (ADTs) and type classes
- Use for-comprehensions for sequential effects
- Leverage Cats Effect type classes (Sync, Async, Concurrent, etc.)

### 2. Cats Effect Best Practices
- Use `IO` for all effectful operations
- Properly handle resource acquisition/release with `Resource`
- Use `Ref` for concurrent mutable state
- Use `Deferred` for synchronization
- Properly propagate cancellation
- Use `Stream` from fs2 for streaming operations
- Never use blocking operations without proper thread pool management

### 3. Error Handling
- Use `Either`, `EitherT`, or `IO.raiseError` for error handling
- Define custom error ADTs for domain errors
- Avoid throwing exceptions; use typed errors
- Use `ApplicativeError` and `MonadError` for polymorphic error handling

### 4. Code Organization
- Separate pure domain logic from effects
- Use tagless final pattern for abstracting effects
- Define algebras (interfaces) for major components
- Implement interpreters for algebras
- Keep modules small and focused

### 5. Documentation & Learning
- **Every class, trait, and object must have ScalaDoc comments**
- **Every public method must have ScalaDoc with parameter descriptions**
- **Complex logic should have inline comments explaining the why, not the what**
- Include code examples in ScalaDoc where helpful
- Document type class constraints and their purpose
- Explain non-obvious functional patterns

### 6. Testing
- Write unit tests using MUnit or ScalaTest
- Use Cats Effect TestControl for time-based testing
- Mock external services appropriately
- Test error cases thoroughly

### 7. Configuration
- Use YAML for application configuration
- Support environment variable overrides
- Use case classes with circe-yaml for config parsing
- Validate configuration at startup

### 8. Kubernetes & Docker Considerations
- Application must be stateless
- Support configuration via mounted secret files
- Use environment variables for runtime configuration
- Proper signal handling for graceful shutdown
- Health check endpoints

### 9. Code Style
- Follow Scala 3 conventions (indent-based syntax where appropriate)
- Use meaningful variable names
- Keep functions small and focused
- Prefer pattern matching over if/else chains
- Use extension methods for enhancing types

## Forbidden Patterns
- ❌ No `var` declarations
- ❌ No `null` values
- ❌ No exceptions for control flow
- ❌ No `asInstanceOf` or type casting
- ❌ No direct thread manipulation
- ❌ No `Await.result` or blocking without proper context
- ❌ No `println` for logging (use proper logging library)

## Required Patterns
- ✅ Use `Option` instead of null
- ✅ Use `Either` or validated types for errors
- ✅ Use `Resource` for resource management
- ✅ Use type classes for polymorphism
- ✅ Use ADTs for domain modeling
- ✅ Use proper logging (log4cats)

## Dependency Management
- Use sbt for build management
- Keep dependencies up to date
- Prefer pure FP libraries from Typelevel ecosystem
- Document why each dependency is needed

## Commit Messages
- Use conventional commits format
- Reference issues when applicable
- Keep messages clear and descriptive
