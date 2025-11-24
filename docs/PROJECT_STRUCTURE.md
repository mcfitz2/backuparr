# Project Structure

This document explains the organization of the Backuparr codebase.

## Directory Layout

```
backuparr/
├── src/
│   ├── main/
│   │   ├── scala/com/backuparr/
│   │   │   ├── Main.scala                    # Application entry point
│   │   │   ├── algebras/                     # Core trait definitions (interfaces)
│   │   │   │   ├── ArrClient.scala          # *arr API client algebra
│   │   │   │   ├── BackupManager.scala      # Backup orchestration algebra
│   │   │   │   ├── RetentionManager.scala   # Retention policy algebra
│   │   │   │   ├── S3Client.scala           # S3 operations algebra
│   │   │   │   └── Scheduler.scala          # Backup scheduling algebra
│   │   │   ├── config/                       # Configuration models and loading
│   │   │   │   ├── Config.scala             # Configuration case classes
│   │   │   │   └── ConfigLoader.scala       # YAML configuration loader
│   │   │   ├── domain/                       # Domain models and types
│   │   │   │   └── Models.scala             # Core domain types and ADTs
│   │   │   └── impl/                         # Implementations (to be created)
│   │   │       ├── ArrClientImpl.scala      # HTTP-based *arr client
│   │   │       ├── BackupManagerImpl.scala  # Backup orchestration logic
│   │   │       ├── RetentionManagerImpl.scala # Retention policy implementation
│   │   │       ├── S3ClientImpl.scala       # S3 client implementation
│   │   │       └── SchedulerImpl.scala      # Cron-based scheduler
│   │   └── resources/
│   │       └── logback.xml                   # Logging configuration
│   └── test/
│       └── scala/com/backuparr/
│           ├── config/
│           │   └── ConfigLoaderSpec.scala    # Configuration tests
│           └── ...                           # More tests to be added
├── project/
│   ├── build.properties                      # sbt version
│   └── plugins.sbt                           # sbt plugins
├── build.sbt                                 # Build configuration
├── config.example.yaml                       # Example configuration file
├── s3-credentials.example.yaml              # Example S3 credentials
├── DESIGN.md                                 # Architectural design document
├── README.md                                 # Project overview
└── .github/
    └── copilot-instructions.md              # Development guidelines
```

## Package Organization

### `com.backuparr`
- **Main.scala**: Application entry point using IOApp

### `com.backuparr.domain`
- Pure domain models and types
- No dependencies on Cats Effect or other libraries
- Algebraic Data Types (ADTs) for type safety
- Examples: `BackupId`, `BackupStatus`, `BackupError`, `ArrType`

### `com.backuparr.config`
- Configuration case classes
- ConfigLoader for loading and validating YAML configs
- Circe decoders for parsing configuration

### `com.backuparr.algebras`
- Trait definitions for all major services
- Uses tagless final pattern (traits with `F[_]` type parameter)
- No implementations, just interfaces
- Examples: `ArrClient[F[_]]`, `S3Client[F[_]]`, `BackupManager[F[_]]`

### `com.backuparr.impl` (to be created)
- Concrete implementations of algebras
- HTTP clients, business logic, etc.
- Each implementation is for a specific effect type (typically `IO`)

## Key Architectural Patterns

### Tagless Final
All major services are defined as algebras (traits with `F[_]`):
```scala
trait ArrClient[F[_]]:
  def requestBackup(instance: ArrInstanceConfig): F[BackupId]
```

This allows:
- Testing with different effect types
- Swapping implementations
- Composing operations generically

### Opaque Types
Types like `BackupId` and `S3Uri` use Scala 3's opaque types:
```scala
opaque type BackupId = String
```

Benefits:
- Type safety (can't mix up BackupId and regular String)
- Zero runtime cost
- Better API clarity

### ADTs for Domain Modeling
Enums and sealed traits represent domain concepts:
```scala
enum BackupStatus:
  case Pending
  case Downloading
  case Completed(uri: S3Uri, timestamp: Instant, size: Long)
  case Failed(error: BackupError, timestamp: Instant)
```

Benefits:
- Exhaustive pattern matching
- Impossible states are unrepresentable
- Clear domain modeling

### Resource Management
Using Cats Effect's `Resource` for managing resources:
```scala
val app = for
  httpClient <- EmberClientBuilder.default[IO].build  // Auto-closed
  arrClient <- Resource.eval(ArrClient.make[IO](httpClient))
yield arrClient
```

Benefits:
- Automatic cleanup
- Composable
- Exception-safe

## Next Steps

The foundation is now in place. Next steps for implementation:

1. **Implement ArrClientImpl**: HTTP client for *arr APIs
2. **Implement S3ClientImpl**: S3 upload/download/list operations
3. **Implement RetentionManagerImpl**: Retention policy logic
4. **Implement BackupManagerImpl**: Orchestrate backup lifecycle
5. **Implement SchedulerImpl**: Cron-based scheduling
6. **Add comprehensive tests**: Unit and integration tests
7. **Create Docker image**: Containerize the application
8. **Create Kubernetes manifests**: Deploy to K8s

## Testing

Run tests with:
```bash
sbt test
```

Run specific test:
```bash
sbt "testOnly com.backuparr.config.ConfigLoaderSpec"
```

## Building

Compile the project:
```bash
sbt compile
```

Run the application:
```bash
sbt run
```

Create Docker image:
```bash
sbt docker:publishLocal
```

## Learning Resources

- [Cats Effect Documentation](https://typelevel.org/cats-effect/)
- [Tagless Final](https://www.youtube.com/watch?v=IhVdU4Xiz2U)
- [Functional Programming in Scala](https://www.manning.com/books/functional-programming-in-scala-second-edition)
- [Scala 3 Book](https://docs.scala-lang.org/scala3/book/introduction.html)
