/**
 * @file JavaException.h
 * JavaException class declaration
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
#ifndef __JAVA_EXCEPTION_H__
#define __JAVA_EXCEPTION_H__

#include <sstream>
#include <stdexcept>
#include <errno.h>
#include <string.h>
#include "JavaDefs.h"
#include "JNIUtil.h"

namespace cclient {
namespace jni {

/**
 * Purpose: Java Exception. Can gather the message from the JVM if need be.
 */
class JavaException : public std::exception {
 public:
  // Constructor
  /*!
   * Create a new JavaException
   */
  JavaException(std::string errorMsg)
      :
      message_(std::move(errorMsg)) {
  }

  // Destructor
  virtual ~JavaException() noexcept {
  }
  virtual const char* what() const noexcept {
    return message_.c_str();
  }

 private:
  // JavaException detailed information
  std::string message_;

};

static std::string getMessage(JNIEnv *env, jthrowable throwable) {

  jclass clazz = env->GetObjectClass(throwable);

  jmethodID getMessage = env->GetMethodID(clazz, "toString", "()Ljava/lang/String;");
  if (getMessage == nullptr) {
    return "";
  }
  jstring message = (jstring) env->CallObjectMethod(throwable, getMessage);
  if (env->ExceptionOccurred()) {
    env->ExceptionClear();
    return JVM_ERROR_MSG;
  }
  if (message) {
    // do whatever with mstr
    std::string excp = JniStringToUTF(env, message);
    env->DeleteLocalRef(message);
    env->DeleteLocalRef(clazz);
    return excp;
  }
  return "";
}

/**
 * Static function to throw the exception if one exists within the ENV.
 * @param env Requires an environment to check
 */
static inline void ThrowIf(JNIEnv *env) {
  if (env->ExceptionCheck()) {
    jthrowable throwable = env->ExceptionOccurred();
    env->ExceptionClear();
    auto message = getMessage(env, throwable);
    throw JavaException(message);
  }
}

static inline void ThrowJava(JNIEnv *env, const char *message) {
  env->ExceptionClear();
  env->ThrowNew(env->FindClass(EXCEPTION_CLASS), message);
}

} /* namespace jni */
} /* namespace cclient */

/**
 * MACROS can make code look worse and more difficult to develop -- but this is a simple
 * if that has no contrary result path.
 */
#define THROW_IF_NULL(expr, env, message) if (UNLIKELY(expr == nullptr)) cclient::jni::ThrowJava(env,message)

#define THROW_IF(expr, env, message) if (UNLIKELY(expr)) cclient::jni::ThrowJava(env,message)

#endif
