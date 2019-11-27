/**
 * @file LoggerConfiguration.h
 * Logger class declaration
 * This is a C++ wrapper for spdlog, a lightweight C++ logging library
 *
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#ifndef __LOGGER_CONFIGURATION_H__
#define __LOGGER_CONFIGURATION_H__

#include <map>
#include <mutex>
#include <string>
#include "spdlog/spdlog.h"
#include "spdlog/formatter.h"

#include "Logger.h"
#include "utils/ClassUtils.h"
#include "Properties.h"

namespace logging {

namespace internal {
struct LoggerNamespace {
  spdlog::level::level_enum level;
  bool has_level;
  std::vector<std::shared_ptr<spdlog::sinks::sink>> sinks;
  std::map<std::string, std::shared_ptr<LoggerNamespace>> children;

  LoggerNamespace()
      : level(spdlog::level::off),
        has_level(false),
        sinks(std::vector<std::shared_ptr<spdlog::sinks::sink>>()),
        children(std::map<std::string, std::shared_ptr<LoggerNamespace>>()) {
  }
};
}
;

class LoggerProperties : public Properties {
 public:
  LoggerProperties()
      : Properties("Logger properties") {
  }
  /**
   * Gets all keys that start with the given prefix and do not have a "." after the prefix and "." separator.
   *
   * Ex: with type argument "appender"
   * you would get back a property of "appender.rolling" but not "appender.rolling.file_name"
   */
  std::vector<std::string> get_keys_of_type(const std::string &type);

  /**
   * Registers a sink with the given name. This allows for programmatic definition of sinks.
   */
  void add_sink(const std::string &name, std::shared_ptr<spdlog::sinks::sink> sink) {
    sinks_[name] = sink;
  }
  std::map<std::string, std::shared_ptr<spdlog::sinks::sink>> initial_sinks() {
    return sinks_;
  }

  static const char* appender_prefix;
  static const char* logger_prefix;
 private:
  std::map<std::string, std::shared_ptr<spdlog::sinks::sink>> sinks_;
};

class LoggerConfiguration {
 public:
  /**
   * Gets the current log configuration
   */
  static LoggerConfiguration& getConfiguration() {
    static LoggerConfiguration logger_configuration;
    return logger_configuration;
  }

  static void enableLogger(){
    getConfiguration().enableLogging();
  }

  static void enableTraceLogger(){
      getConfiguration().enableLogging(LOG_LEVEL::trace);
    }

  static std::unique_ptr<LoggerConfiguration> newInstance() {
    return std::unique_ptr<LoggerConfiguration>(new LoggerConfiguration());
  }

  void disableLogging() {
    controller_->setEnabled(false);
  }

  void enableLogging(LOG_LEVEL level = LOG_LEVEL::debug, bool use_stdout = true) {
    controller_->setEnabled(true);
    if (use_stdout) {
      auto logger_properties = std::make_shared<LoggerProperties>();
      logger_properties->set("spdlog.pattern", "[%Y-%m-%d %H:%M:%S.%e] [%n] [%l] %v");
      logger_properties->set("spdlog.shorten_names", "true");
      logger_properties->set("appender.stdout", "stdout");
      std::string strlevel = "DEBUG";
      switch (level) {
        case LOG_LEVEL::info:
          strlevel = "INFO";
          break;
        case LOG_LEVEL::debug:
          strlevel = "DEBUG";
          break;
        case LOG_LEVEL::trace:
          strlevel = "TRACE";
          break;
        case LOG_LEVEL::warn:
          strlevel = "WARN";
          break;
        case LOG_LEVEL::err:
          strlevel = "ERROR";
          break;
        case LOG_LEVEL::critical:
          strlevel = "ERROR";
          break;
        case LOG_LEVEL::off:
          strlevel = "OFF";
          break;
      }
      logger_properties->set("logger.root", strlevel + ",stdout");
      initialize(logger_properties);
    }

  }

  bool shortenClassNames() const {
    return shorten_names_;
  }
  /**
   * (Re)initializes the logging configuation with the given logger properties.
   */
  void initialize(const std::shared_ptr<LoggerProperties> &logger_properties);

  /**
   * Can be used to get arbitrarily named Logger, LoggerFactory should be preferred within a class.
   */
  std::shared_ptr<Logger> getLogger(const std::string &name);
  static const char *spdlog_default_pattern;
 protected:
  static std::shared_ptr<internal::LoggerNamespace> initialize_namespaces(const std::shared_ptr<LoggerProperties> &logger_properties);
  static std::shared_ptr<spdlog::logger> get_logger(std::shared_ptr<Logger> logger, const std::shared_ptr<internal::LoggerNamespace> &root_namespace, const std::string &name,
                                                    std::shared_ptr<spdlog::formatter> formatter, bool remove_if_present = false);
 private:
  static std::shared_ptr<internal::LoggerNamespace> create_default_root();

  class LoggerImpl : public Logger {
   public:
    explicit LoggerImpl(const std::string &name, const std::shared_ptr<LoggerControl> &controller, const std::shared_ptr<spdlog::logger> &delegate)
        : Logger(delegate, controller),
          name(name) {
    }

    void set_delegate(std::shared_ptr<spdlog::logger> delegate) {
      std::lock_guard<std::mutex> lock(mutex_);
      delegate_ = delegate;
    }
    const std::string name;

  };

  LoggerConfiguration();
  std::shared_ptr<internal::LoggerNamespace> root_namespace_;
  std::vector<std::shared_ptr<LoggerImpl>> loggers;
  std::shared_ptr<spdlog::formatter> formatter_;
  std::mutex mutex;
  std::shared_ptr<LoggerImpl> logger_ = nullptr;
  std::shared_ptr<LoggerControl> controller_;
  bool shorten_names_;

};

template<typename T>
class LoggerFactory {
 public:
  /**
   * Gets an initialized logger for the template class.
   */
  static std::shared_ptr<Logger> getLogger() {
    static std::shared_ptr<Logger> logger = LoggerConfiguration::getConfiguration().getLogger(ClassUtils::getClassName<T>());
    return logger;
  }

  static std::shared_ptr<Logger> getAliasedLogger(const std::string &alias) {
    std::shared_ptr<Logger> logger = LoggerConfiguration::getConfiguration().getLogger(alias);
    return logger;
  }
};

} /* namespace logging */

#endif
