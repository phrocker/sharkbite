/**
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
#include "logging/Properties.h"

#include <string>

#include "logging/LoggerConfiguration.h"
#include "utils/StringUtils.h"

#define TRACE_BUFFER_SIZE 512

Properties::Properties(const std::string &name)
    : logger_(logging::LoggerFactory<Properties>::getLogger()), name_(name) {}

// Get the config value
bool Properties::get(const std::string &key, std::string &value) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = properties_.find(key);

  if (it != properties_.end()) {
    value = it->second;
    return true;
  } else {
    return false;
  }
}

bool Properties::get(const std::string &key, const std::string &alternate_key,
                     std::string &value) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = properties_.find(key);

  if (it == properties_.end()) {
    it = properties_.find(alternate_key);
    if (it != properties_.end()) {
      logger_->log_warn(
          "%s is an alternate property that may not be supported in future "
          "releases. Please use %s instead.",
          alternate_key, key);
    }
  }

  if (it != properties_.end()) {
    value = it->second;
    return true;
  } else {
    return false;
  }
}

int Properties::getInt(const std::string &key, int default_value) {
  std::lock_guard<std::mutex> lock(mutex_);
  auto it = properties_.find(key);

  if (it != properties_.end()) {
    return std::atol(it->second.c_str());
  } else {
    return default_value;
  }
}

// Parse one line in configure file like key=value
bool Properties::parseConfigureFileLine(char *buf, std::string &prop_key,
                                        std::string &prop_value) {
  char *line = buf;

  while ((line[0] == ' ') || (line[0] == '\t')) ++line;

  char first = line[0];
  if ((first == '\0') || (first == '#') || (first == '[') || (first == '\r') ||
      (first == '\n') || (first == '=')) {
    return true;
  }

  char *equal = strchr(line, '=');
  if (equal == NULL) {
    return false;  // invalid property as this is not a comment or property line
  }

  equal[0] = '\0';
  std::string key = line;

  equal++;
  while ((equal[0] == ' ') || (equal[0] == '\t')) ++equal;

  first = equal[0];
  if ((first == '\0') || (first == '\r') || (first == '\n')) {
    return true;  // empty properties are okay
  }

  std::string value = equal;
  value = utils::StringUtils::replaceEnvironmentVariables(value);
  prop_key = utils::StringUtils::trimRight(key);
  prop_value = utils::StringUtils::trimRight(value);
  return true;
}

// Load Configure File
void Properties::loadConfigurationFile(const char *fileName) {
  if (NULL == fileName) {
    logger_->log_error("Configuration file path for %s is a nullptr!",
                       getName().c_str());
    return;
  }

  std::string adjustedFilename = getHome();
  // perform a naive determination if this is a relative path
  if (fileName[0] != FILE_SEPARATOR) {
    adjustedFilename += FILE_SEPARATOR;
  }

  adjustedFilename += fileName;

  const char *path = NULL;

#ifndef WIN32
  char full_path[PATH_MAX];
  path = realpath(adjustedFilename.c_str(), full_path);
#else
  path = adjustedFilename.c_str();
#endif
  logger_->log_info(
      "Using configuration file to load configuration for %s from %s (located "
      "at %s)",
      getName().c_str(), fileName, path);

  properties_file_ = path;

  std::ifstream file(path, std::ifstream::in);
  if (!file.good()) {
    logger_->log_error("load configure file failed %s", path);
    return;
  }
  this->clear();

  char buf[TRACE_BUFFER_SIZE];
  for (file.getline(buf, TRACE_BUFFER_SIZE); file.good();
       file.getline(buf, TRACE_BUFFER_SIZE)) {
    std::string key, value;
    if (parseConfigureFileLine(buf, key, value)) {
      set(key, value);
    }
  }
  dirty_ = false;
}

bool Properties::validateConfigurationFile(const std::string &configFile) {
  std::ifstream file(configFile, std::ifstream::in);
  if (!file.good()) {
    logger_->log_error("Failed to load configuration file %s to configure %s",
                       configFile, getName().c_str());
    return false;
  }

  char buf[TRACE_BUFFER_SIZE];
  for (file.getline(buf, TRACE_BUFFER_SIZE); file.good();
       file.getline(buf, TRACE_BUFFER_SIZE)) {
    std::string key, value;
    if (!parseConfigureFileLine(buf, key, value)) {
      logger_->log_error(
          "While loading configuration for %s found invalid line: %s",
          getName().c_str(), buf);
      return false;
    }
  }
  return true;
}

// Parse Command Line
void Properties::parseCommandLine(int argc, char **argv) {
  int i;
  bool keyFound = false;
  std::string key, value;

  for (i = 1; i < argc; i++) {
    if (argv[i][0] == '-' && argv[i][1] != '\0') {
      keyFound = true;
      key = &argv[i][1];
      continue;
    }
    if (keyFound) {
      value = argv[i];
      set(key, value);
      keyFound = false;
    }
  }
  return;
}
