# Contributing to Backuparr

Thank you for your interest in contributing to Backuparr! This document provides guidelines and instructions for contributing.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [Coding Guidelines](#coding-guidelines)
- [Testing](#testing)
- [Submitting Changes](#submitting-changes)
- [Release Process](#release-process)

## Code of Conduct

This project adheres to a code of conduct that promotes a welcoming and inclusive environment. Please be respectful and professional in all interactions.

## Getting Started

### Prerequisites

- JDK 21+
- sbt 1.9.0+
- Docker and Docker Compose (for integration tests)
- Git
- A GitHub account

### Fork and Clone

1. Fork the repository on GitHub
2. Clone your fork:
   ```bash
   git clone https://github.com/YOUR_USERNAME/backuparr.git
   cd backuparr
   ```
3. Add upstream remote:
   ```bash
   git remote add upstream https://github.com/ORIGINAL_OWNER/backuparr.git
   ```

## Development Setup

### Initial Setup

```bash
# Compile the project
sbt compile

# Run unit tests
sbt "testOnly -- --exclude-tags=integration"

# Start test infrastructure
docker-compose -f docker-compose.test.yml up -d

# Run integration tests
sbt integrationTest

# Stop test infrastructure
docker-compose -f docker-compose.test.yml down
```

### IDE Setup

#### IntelliJ IDEA

1. Install Scala plugin
2. Import project as sbt project
3. Set JDK to 21
4. Enable "Use sbt shell for build and import"

#### VS Code

1. Install Metals extension
2. Open project folder
3. Metals will auto-configure
4. Run "Metals: Import build" if needed

## Coding Guidelines

This project follows strict functional programming principles. See [`.github/copilot-instructions.md`](.github/copilot-instructions.md) for complete guidelines.

### Core Principles

1. **Pure Functional Programming**
   - All functions must be pure (no side effects)
   - Use immutable data structures exclusively
   - Effects must be tracked in `IO` or `F[_]`
   - No `var`, `null`, or exceptions for control flow

2. **Type Safety**
   - Use ADTs (sealed traits) for domain modeling
   - Avoid `asInstanceOf` and type casting
   - Leverage Scala 3's advanced type features

3. **Resource Management**
   - Use `Resource` for all resource acquisition
   - Ensure proper cleanup in all code paths
   - No manual `.close()` calls

4. **Error Handling**
   - Use `Either`, `EitherT`, or `IO.raiseError`
   - Define custom error ADTs
   - Never throw exceptions

### Code Style

```scala
// ✅ GOOD: Pure function with effect tracking
def uploadBackup[F[_]: Async](file: Path, bucket: String): F[S3Uri] =
  for
    _    <- Logger[F].info(s"Uploading $file to $bucket")
    uri  <- s3Client.upload(file, bucket)
    _    <- Logger[F].info(s"Upload complete: $uri")
  yield uri

// ❌ BAD: Side effects, mutable state, exceptions
def uploadBackup(file: Path, bucket: String): String = {
  var result = ""
  try {
    println(s"Uploading $file")  // Side effect!
    result = s3Client.upload(file, bucket)  // Mutable!
  } catch {
    case e: Exception => throw e  // Exception for control flow!
  }
  result
}
```

### Documentation

Every public API must have ScalaDoc:

```scala
/**
 * Uploads a backup file to S3 storage.
 *
 * This function handles the complete upload workflow including:
 * - Stream creation from the file
 * - Multi-part upload for large files
 * - Metadata attachment
 * - Retry logic for transient failures
 *
 * @param file The local file path to upload
 * @param bucket The target S3 bucket name
 * @tparam F The effect type (must have Async instance)
 * @return The S3 URI of the uploaded file
 * @throws S3UploadError if upload fails after retries
 *
 * Example:
 * {{{
 *   uploadBackup[IO](
 *     file = Paths.get("/tmp/sonarr-backup.zip"),
 *     bucket = "my-backups"
 *   ).flatMap { uri =>
 *     IO.println(s"Uploaded to: $uri")
 *   }
 * }}}
 */
def uploadBackup[F[_]: Async](file: Path, bucket: String): F[S3Uri]
```

### Testing

Every new feature must include tests:

```scala
// Unit test example
test("CronExpression.parse - valid daily schedule"):
  val result = CronExpression.parse("0 2 * * *")
  assert(result.isRight)
  assertEquals(result.map(_.toString), Right("0 2 * * *"))

// Integration test example (tagged)
test("S3Client - upload and verify file exists".tag(IntegrationTest)):
  val testFile = createTestFile("test.txt", "content")
  for
    _      <- s3Client.uploadFile(testFile, "test-key")
    exists <- s3Client.objectExists("test-key")
    _      <- s3Client.deleteObject("test-key")
  yield assertEquals(exists, true)
```

## Testing

### Test Categories

1. **Unit Tests** - Fast, no external dependencies
   ```bash
   sbt "testOnly -- --exclude-tags=integration"
   ```

2. **Integration Tests** - Require Docker infrastructure
   ```bash
   # Start infrastructure
   docker-compose -f docker-compose.test.yml up -d
   
   # Run tests
   sbt integrationTest
   
   # Cleanup
   docker-compose -f docker-compose.test.yml down -v
   ```

3. **E2E Tests** - Full system with real services
   ```bash
   sbt "testOnly com.backuparr.integration.E2EIntegrationSpec"
   ```

### Writing Tests

Tests use MUnit with Cats Effect integration:

```scala
class MyFeatureSpec extends CatsEffectSuite:
  
  test("feature does something useful"):
    for
      result <- MyFeature.doSomething()
      _      <- IO(assertEquals(result, expectedValue))
    yield ()

  test("feature handles errors correctly"):
    MyFeature.doSomethingBad()
      .attempt
      .map { result =>
        assert(result.isLeft)
        assert(result.left.exists(_.isInstanceOf[MyError]))
      }
```

### Test Coverage

Aim for:
- Unit tests: 80%+ coverage
- Integration tests: All critical paths
- E2E tests: Major workflows

Run coverage report:
```bash
sbt coverage test coverageReport
open target/scala-3.3.1/scoverage-report/index.html
```

## Submitting Changes

### Branch Naming

Use descriptive branch names:
- `feature/add-backup-encryption`
- `fix/retention-calculation-bug`
- `docs/update-configuration-guide`
- `refactor/simplify-scheduler`

### Commit Messages

Follow conventional commits:

```
feat: add support for Whisparr backups

- Implement Whisparr client with v3 API
- Add Whisparr configuration to domain models
- Include integration tests for Whisparr
- Update documentation with Whisparr examples

Closes #123
```

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`

### Pull Request Process

1. **Create a feature branch**:
   ```bash
   git checkout -b feature/my-awesome-feature
   ```

2. **Make your changes**:
   - Follow coding guidelines
   - Add tests
   - Update documentation
   - Ensure all tests pass

3. **Commit your changes**:
   ```bash
   git add .
   git commit -m "feat: add my awesome feature"
   ```

4. **Push to your fork**:
   ```bash
   git push origin feature/my-awesome-feature
   ```

5. **Create Pull Request**:
   - Go to GitHub and create PR
   - Fill out the PR template
   - Link related issues
   - Request review

### PR Checklist

Before submitting, ensure:

- [ ] Code follows functional programming guidelines
- [ ] All functions have ScalaDoc comments
- [ ] Unit tests added for new functionality
- [ ] Integration tests added if applicable
- [ ] All tests pass locally
- [ ] Documentation updated (README, GETTING_STARTED, etc.)
- [ ] Commit messages follow conventional commits
- [ ] No merge conflicts with main branch

### Code Review

All PRs require review. Reviewers will check:

1. **Correctness**: Does it work as intended?
2. **Style**: Does it follow FP guidelines?
3. **Tests**: Are there sufficient tests?
4. **Documentation**: Is it well documented?
5. **Performance**: Are there any performance concerns?

Address review feedback by:
- Pushing additional commits to your branch
- Explaining your reasoning if you disagree
- Being open to suggestions and learning

## Release Process

### Version Numbering

We use Semantic Versioning (SemVer):
- `MAJOR.MINOR.PATCH`
- Example: `1.2.3`

Increment:
- **MAJOR**: Breaking changes
- **MINOR**: New features (backward compatible)
- **PATCH**: Bug fixes (backward compatible)

### Creating a Release

1. Update version in `build.sbt`:
   ```scala
   version := "1.2.0"
   ```

2. Update CHANGELOG.md:
   ```markdown
   ## [1.2.0] - 2025-11-23
   
   ### Added
   - Support for Whisparr backups
   - Backup encryption feature
   
   ### Fixed
   - Retention policy calculation bug
   ```

3. Create release commit:
   ```bash
   git add build.sbt CHANGELOG.md
   git commit -m "chore: release v1.2.0"
   git tag -a v1.2.0 -m "Release v1.2.0"
   ```

4. Push with tags:
   ```bash
   git push origin main --tags
   ```

5. GitHub Actions will automatically build and publish

## Development Tips

### Running Locally

```bash
# Watch mode (recompile on changes)
sbt ~compile

# Run from sbt
sbt "run config.yaml"

# Assembly and run
sbt assembly
java -jar target/scala-3.3.1/backuparr-assembly-*.jar config.yaml
```

### Debugging

Add debug logging:
```scala
import org.typelevel.log4cats.Logger

def myFunction[F[_]: Logger: Async]: F[Unit] =
  for
    _ <- Logger[F].debug("Starting myFunction")
    _ <- doSomething()
    _ <- Logger[F].debug("Completed myFunction")
  yield ()
```

### Hot Reload

Use sbt's triggered execution:
```bash
sbt
> ~testOnly MySpec
```

## Project Structure

```
backuparr/
├── src/
│   ├── main/scala/com/backuparr/
│   │   ├── algebras/       # Interfaces (traits)
│   │   ├── config/         # Configuration models
│   │   ├── domain/         # Domain models (ADTs)
│   │   ├── http/           # HTTP routes and server
│   │   ├── impl/           # Implementations
│   │   └── Main.scala      # Application entry point
│   └── test/scala/com/backuparr/
│       ├── config/         # Config tests
│       ├── impl/           # Unit tests
│       └── integration/    # Integration tests
├── build.sbt               # Build configuration
├── project/                # sbt project files
├── docker-compose.yml      # Local development
├── docker-compose.test.yml # Integration tests
└── Dockerfile              # Production image
```

## Getting Help

- **Questions**: Open a [Discussion](https://github.com/yourusername/backuparr/discussions)
- **Bugs**: Open an [Issue](https://github.com/yourusername/backuparr/issues)
- **Chat**: Join our Discord/Slack (if available)
- **Email**: maintainer@example.com

## Recognition

Contributors will be recognized in:
- README.md contributors section
- Release notes
- CONTRIBUTORS.md file

Thank you for contributing to Backuparr! 🎉
