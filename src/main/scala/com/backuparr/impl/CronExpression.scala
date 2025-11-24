package com.backuparr.impl

import java.time.{DayOfWeek, LocalDateTime, ZoneId}
import scala.util.Try

/**
 * Simple cron expression parser and evaluator.
 * 
 * Supports standard cron format: minute hour day month day-of-week
 * 
 * Supported syntax:
 * - * (any value)
 * - specific value (e.g., 5)
 * - list (e.g., 1,3,5)
 * - range (e.g., 1-5)
 * - step (e.g., star-slash-5 or 0-30/5)
 * 
 * Examples:
 * - "0 2 * * *"     - Daily at 2:00 AM
 * - "0 star-slash-6 * * *"   - Every 6 hours
 * - "0 0 * * 0"     - Weekly on Sunday at midnight
 * - "0 0 1 * *"     - Monthly on 1st at midnight
 * - "star-slash-15 * * * *"  - Every 15 minutes
 * 
 * @param minute Minute field (0-59)
 * @param hour Hour field (0-23)
 * @param day Day of month field (1-31)
 * @param month Month field (1-12)
 * @param dayOfWeek Day of week field (0-7, 0 and 7 are Sunday)
 */
case class CronExpression(
  minute: CronField,
  hour: CronField,
  day: CronField,
  month: CronField,
  dayOfWeek: CronField
):
  /**
   * Check if this cron expression matches the given datetime.
   */
  def matches(dateTime: LocalDateTime): Boolean =
    val minuteMatch = minute.matches(dateTime.getMinute)
    val hourMatch = hour.matches(dateTime.getHour)
    val monthMatch = month.matches(dateTime.getMonthValue)
    
    // Day matching: if both day and dayOfWeek are specified (not *), use OR
    // If only one is specified, use that one
    val dayMatch = (day, dayOfWeek) match
      case (CronField.Any, CronField.Any) => true
      case (CronField.Any, _) => dayOfWeekMatches(dateTime)
      case (_, CronField.Any) => day.matches(dateTime.getDayOfMonth)
      case _ => day.matches(dateTime.getDayOfMonth) || dayOfWeekMatches(dateTime)
    
    minuteMatch && hourMatch && monthMatch && dayMatch
  
  /**
   * Special handling for day-of-week because cron uses 0-7 for Sunday-Saturday.
   */
  private def dayOfWeekMatches(dateTime: LocalDateTime): Boolean =
    val javaDow = dateTime.getDayOfWeek.getValue // Monday=1, Sunday=7
    val cronDow = if javaDow == 7 then 0 else javaDow // Convert Sunday to 0
    dayOfWeek.matches(cronDow) || dayOfWeek.matches(7) && cronDow == 0
  
  /**
   * Calculate the next execution time after the given datetime.
   * 
   * This advances minute by minute until a match is found.
   * For efficiency, we limit the search to 4 years (very conservative).
   */
  def nextExecution(from: LocalDateTime): Option[LocalDateTime] =
    // Start from the next minute
    var current = from.plusMinutes(1).withSecond(0).withNano(0)
    val maxIterations = 4 * 365 * 24 * 60 // 4 years worth of minutes
    
    var iterations = 0
    while iterations < maxIterations do
      if matches(current) then
        return Some(current)
      current = current.plusMinutes(1)
      iterations += 1
    
    None // No match found (shouldn't happen for valid cron expressions)

/**
 * Represents a single field in a cron expression.
 */
sealed trait CronField:
  def matches(value: Int): Boolean

object CronField:
  /** Matches any value (*) */
  case object Any extends CronField:
    def matches(value: Int): Boolean = true
  
  /** Matches a specific value */
  case class Exact(value: Int) extends CronField:
    def matches(v: Int): Boolean = v == value
  
  /** Matches any value in the list */
  case class List(values: Set[Int]) extends CronField:
    def matches(v: Int): Boolean = values.contains(v)
  
  /** Matches values in a range */
  case class Range(start: Int, end: Int) extends CronField:
    def matches(v: Int): Boolean = v >= start && v <= end
  
  /** Matches values with a step (e.g., every 5 minutes, 10-30 by 5) */
  case class Step(base: CronField, step: Int) extends CronField:
    def matches(v: Int): Boolean =
      base.matches(v) && (v % step == 0)

object CronExpression:
  /**
   * Parse a cron expression string.
   * 
   * Format: minute hour day month day-of-week
   * 
   * Returns None if parsing fails.
   */
  def parse(expression: String): Option[CronExpression] =
    Try {
      val parts = expression.trim.split("\\s+")
      require(parts.length == 5, s"Cron expression must have 5 fields, got ${parts.length}")
      
      CronExpression(
        minute = parseField(parts(0), 0, 59),
        hour = parseField(parts(1), 0, 23),
        day = parseField(parts(2), 1, 31),
        month = parseField(parts(3), 1, 12),
        dayOfWeek = parseField(parts(4), 0, 7)
      )
    }.toOption
  
  /**
   * Parse a single cron field.
   */
  private def parseField(field: String, min: Int, max: Int): CronField =
    field match
      case "*" => CronField.Any
      
      case s if s.contains("/") =>
        val Array(base, stepStr) = s.split("/")
        val step = stepStr.toInt
        val baseField = if base == "*" then CronField.Any else parseField(base, min, max)
        CronField.Step(baseField, step)
      
      case s if s.contains(",") =>
        val values = s.split(",").map(_.toInt).toSet
        require(values.forall(v => v >= min && v <= max), s"Values must be in range $min-$max")
        CronField.List(values)
      
      case s if s.contains("-") =>
        val Array(startStr, endStr) = s.split("-")
        val start = startStr.toInt
        val end = endStr.toInt
        require(start >= min && end <= max, s"Range must be in $min-$max")
        CronField.Range(start, end)
      
      case s =>
        val value = s.toInt
        require(value >= min && value <= max, s"Value must be in range $min-$max")
        CronField.Exact(value)
