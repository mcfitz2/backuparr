package com.backuparr.impl

import munit.FunSuite

import java.time.LocalDateTime

/**
 * Unit tests for CronExpression parser and evaluator.
 */
class CronExpressionSpec extends FunSuite:
  
  test("parse - valid daily at 2 AM"):
    val cron = CronExpression.parse("0 2 * * *")
    assert(cron.isDefined, "Should parse valid cron expression")
    
    val expr = cron.get
    assert(expr.minute == CronField.Exact(0))
    assert(expr.hour == CronField.Exact(2))
    assert(expr.day == CronField.Any)
    assert(expr.month == CronField.Any)
    assert(expr.dayOfWeek == CronField.Any)
  
  test("parse - every 6 hours"):
    val cron = CronExpression.parse("0 */6 * * *")
    assert(cron.isDefined)
    
    val expr = cron.get
    assert(expr.minute == CronField.Exact(0))
    expr.hour match
      case CronField.Step(CronField.Any, 6) => // Expected
      case other => fail(s"Expected Step(Any, 6), got $other")
  
  test("parse - weekly on Sunday"):
    val cron = CronExpression.parse("0 0 * * 0")
    assert(cron.isDefined)
    
    val expr = cron.get
    assert(expr.dayOfWeek == CronField.Exact(0))
  
  test("parse - monthly on 1st"):
    val cron = CronExpression.parse("0 0 1 * *")
    assert(cron.isDefined)
    
    val expr = cron.get
    assert(expr.day == CronField.Exact(1))
  
  test("parse - list of hours"):
    val cron = CronExpression.parse("0 6,12,18 * * *")
    assert(cron.isDefined)
    
    val expr = cron.get
    expr.hour match
      case CronField.List(values) =>
        assert(values == Set(6, 12, 18))
      case other => fail(s"Expected List, got $other")
  
  test("parse - range"):
    val cron = CronExpression.parse("0 9-17 * * *")
    assert(cron.isDefined)
    
    val expr = cron.get
    expr.hour match
      case CronField.Range(9, 17) => // Expected
      case other => fail(s"Expected Range(9, 17), got $other")
  
  test("parse - invalid expression"):
    val cron = CronExpression.parse("invalid")
    assert(cron.isEmpty, "Should reject invalid cron expression")
  
  test("parse - too few fields"):
    val cron = CronExpression.parse("0 2 *")
    assert(cron.isEmpty, "Should reject expression with too few fields")
  
  test("matches - daily at 2 AM"):
    val cron = CronExpression.parse("0 2 * * *").get
    
    val matching = LocalDateTime.of(2025, 11, 23, 2, 0)
    assert(cron.matches(matching), "Should match 2:00 AM")
    
    val notMatching = LocalDateTime.of(2025, 11, 23, 3, 0)
    assert(!cron.matches(notMatching), "Should not match 3:00 AM")
  
  test("matches - every 6 hours"):
    val cron = CronExpression.parse("0 */6 * * *").get
    
    assert(cron.matches(LocalDateTime.of(2025, 11, 23, 0, 0)), "Should match midnight")
    assert(cron.matches(LocalDateTime.of(2025, 11, 23, 6, 0)), "Should match 6 AM")
    assert(cron.matches(LocalDateTime.of(2025, 11, 23, 12, 0)), "Should match noon")
    assert(cron.matches(LocalDateTime.of(2025, 11, 23, 18, 0)), "Should match 6 PM")
    
    assert(!cron.matches(LocalDateTime.of(2025, 11, 23, 3, 0)), "Should not match 3 AM")
  
  test("matches - weekly on Sunday"):
    val cron = CronExpression.parse("0 0 * * 0").get
    
    // November 23, 2025 is a Sunday
    val sunday = LocalDateTime.of(2025, 11, 23, 0, 0)
    assert(cron.matches(sunday), "Should match Sunday midnight")
    
    // November 24, 2025 is a Monday
    val monday = LocalDateTime.of(2025, 11, 24, 0, 0)
    assert(!cron.matches(monday), "Should not match Monday")
  
  test("nextExecution - daily at 2 AM"):
    val cron = CronExpression.parse("0 2 * * *").get
    
    // From 1 AM, next should be 2 AM same day
    val from1AM = LocalDateTime.of(2025, 11, 23, 1, 0)
    val next = cron.nextExecution(from1AM)
    assert(next.isDefined)
    assert(next.get == LocalDateTime.of(2025, 11, 23, 2, 0))
    
    // From 3 AM, next should be 2 AM next day
    val from3AM = LocalDateTime.of(2025, 11, 23, 3, 0)
    val nextDay = cron.nextExecution(from3AM)
    assert(nextDay.isDefined)
    assert(nextDay.get == LocalDateTime.of(2025, 11, 24, 2, 0))
  
  test("nextExecution - every 15 minutes"):
    val cron = CronExpression.parse("*/15 * * * *").get
    
    val from = LocalDateTime.of(2025, 11, 23, 10, 7)
    val next = cron.nextExecution(from)
    assert(next.isDefined)
    assert(next.get == LocalDateTime.of(2025, 11, 23, 10, 15))
  
  test("CronField.Any - matches all values"):
    assert(CronField.Any.matches(0))
    assert(CronField.Any.matches(59))
    assert(CronField.Any.matches(100))
  
  test("CronField.Exact - matches specific value"):
    val field = CronField.Exact(5)
    assert(field.matches(5))
    assert(!field.matches(4))
    assert(!field.matches(6))
  
  test("CronField.List - matches listed values"):
    val field = CronField.List(Set(1, 3, 5))
    assert(field.matches(1))
    assert(field.matches(3))
    assert(field.matches(5))
    assert(!field.matches(2))
    assert(!field.matches(4))
  
  test("CronField.Range - matches range"):
    val field = CronField.Range(10, 20)
    assert(!field.matches(9))
    assert(field.matches(10))
    assert(field.matches(15))
    assert(field.matches(20))
    assert(!field.matches(21))
  
  test("CronField.Step - matches with step"):
    val field = CronField.Step(CronField.Any, 5)
    assert(field.matches(0))
    assert(field.matches(5))
    assert(field.matches(10))
    assert(field.matches(15))
    assert(!field.matches(1))
    assert(!field.matches(7))
