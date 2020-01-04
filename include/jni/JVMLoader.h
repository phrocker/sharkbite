/**
 * @file ScriptException.h
 * ScriptException class declaration
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
#ifndef INCLUDE_JNI_JVMLOADER_H_
#define INCLUDE_JNI_JVMLOADER_H_

#include <mutex>
#include <atomic>
#include <map>
#include <jni.h>
#include "JavaException.h"
#include "JavaClass.h"

#ifdef _WIN32
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN 1
#endif
// can't include cxxabi
#else
#include <cxxabi.h>
#endif

namespace cclient {
namespace jni {

template<typename T>
static inline std::string getClassName() {
#ifndef WIN32
  char *b = abi::__cxa_demangle(typeid(T).name(), 0, 0, 0);
  if (b == nullptr)
    return std::string();
  std::string name = b;
  std::free(b);
  return name;
#else
  std::string adjusted_name = typeid(T).name();
  // can probably skip class  manually for slightly higher performance
  const std::string clazz = "class ";
  auto haz_clazz = adjusted_name.find(clazz);
  if (haz_clazz == 0)
    adjusted_name = adjusted_name.substr(clazz.length(), adjusted_name.length() - clazz.length());
  return adjusted_name;
#endif
}

/**
 * Purpose and Justification: Provides a mapping function for jfields.
 * Note that jFieldIDs aren't local references, so we don't need to worry
 * about keeping them around. Thus this class provides us a caching mechanism.
 */
class FieldMapping {
 public:
  jfieldID getField(const std::string &clazz, const std::string &fnArg) {
    std::lock_guard<std::mutex> guard(mutex_);
    auto group = map_.find(clazz);
    if (group != map_.end()) {
      auto match = group->second.find(fnArg);
      if (match != group->second.end()) {
        return match->second;
      }
    }
    return nullptr;
  }

  void putField(const std::string &clazz, const std::string &fnArg, jfieldID field) {
    std::lock_guard<std::mutex> guard(mutex_);
    map_[clazz].insert(std::make_pair(fnArg, field));
  }

 private:
  std::mutex mutex_;
  std::map<std::string, std::map<std::string, jfieldID>> map_;
};

typedef jint (*registerNatives_t)(JNIEnv *env, jclass clazz);

jfieldID getPtrField(JNIEnv *env, jobject obj);

template<typename T>
T* getPtr(JNIEnv *env, jobject obj);

template<typename T>
void setPtr(JNIEnv *env, jobject obj, T *t);

/**
 * Purpose and Justification: Provides a singleton reference to a JVM.
 *
 * Since JVMs are singular in reference ( meaning that there cannot be more than one
 * at any given time ), we need to create a singleton access pattern.
 *
 */
class JVMLoader {
 public:

  bool initialized() {
    return initialized_;
  }

  /**
   * Removes a class reference
   * @param name class name
   */
  void remove_class(JNIEnv *env, const std::string &name) {
    std::lock_guard<std::mutex> lock(internal_mutex_);
    auto finder = objects_.find(name);
    if (finder != objects_.end()) {
      auto oldClzz = finder->second;
      JavaClass clazz(oldClzz);
      env->DeleteGlobalRef(clazz.getReference());
    }
    objects_.erase(name);
  }

  static JVMLoader* getInstance() {
    static JVMLoader jvm;
    jvm.initialized_ = true;
    return &jvm;
  }

  template<typename T>
  void setReference(jobject obj, JNIEnv *env, T *t) {
    setPtr(env, obj, t);
  }

  template<typename T>
  T* getReference(JNIEnv *env, jobject obj) {
    return getPtr<T>(env, obj);
  }

  /**
   * Get the pointer field to the
   * This expects nativePtr to exist. I've always used nativePtr because I know others use it across
   * stack overflow. It's a common field, so the hope is that we can potentially access any class
   * in which the native pointer is stored in nativePtr.
   */
  static jfieldID getPtrField(const std::string &className, JNIEnv *env, jobject obj) {
    static std::string fn = "nativePtr", args = "J", lookup = "nativePtrJ";
    auto field = getClassMapping().getField(className, lookup);
    if (field != nullptr) {
      return field;
    }

    jclass c = env->GetObjectClass(obj);
    return env->GetFieldID(c, "nativePtr", "J");
  }

  template<typename T>
  static T* getPtr(JNIEnv *env, jobject obj) {
    jlong handle = env->GetLongField(obj, getPtrField(getClassName<T>(), env, obj));
    return reinterpret_cast<T*>(handle);
  }

  template<typename T>
  static void setPtr(JNIEnv *env, jobject obj, T *t) {
    jlong handle = reinterpret_cast<jlong>(t);
    env->SetLongField(obj, getPtrField(getClassName<T>(), env, obj), handle);
  }

  template<typename T>
  static void putClassMapping(JNIEnv *env, JavaClass &clazz, const std::string &fieldStr, const std::string &arg) {
    auto classref = clazz.getReference();
    auto name = getClassName<T>();
    auto field = env->GetFieldID(classref, fieldStr.c_str(), arg.c_str());
    auto fieldName = fieldStr + arg;
    getClassMapping().putField(name, fieldName, field);

  }

 protected:

  static FieldMapping& getClassMapping() {
    static FieldMapping map;
    return map;
  }

  mutable std::mutex internal_mutex_;

  inline jclass find_class_global(JNIEnv *env, const char *name) {
    jclass c = env->FindClass(name);
    jclass c_global = (jclass) env->NewGlobalRef(c);
    if (!c) {
      std::stringstream ss;
      ss << "Could not find " << name;
      throw std::runtime_error(ss.str());
    }
    return c_global;
  }

 private:

  std::atomic<bool> initialized_;

  std::map<std::string, JavaClass> objects_;

  std::vector<std::string> string_options_;

  JVMLoader() {
    initialized_ = true;
  }

  ~JVMLoader() {

  }
};

} /* namespace jni */
} /* namespace cclient */

#endif /* INCLUDE_JNI_JVMLOADER_H_ */
