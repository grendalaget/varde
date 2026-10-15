plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "games.varde.anchor"
    compileSdk = 35

    defaultConfig {
        applicationId = "games.varde.anchor"
        minSdk = 26
        targetSdk = 35
        // Release CI injects -PvardeVersionName/-PvardeVersionCode; the
        // defaults only apply to local debug builds.
        versionCode = providers.gradleProperty("vardeVersionCode").map(String::toInt).orElse(1).get()
        versionName = providers.gradleProperty("vardeVersionName").orElse("0.1.0").get()
    }

    // The agent + mesh ship as lib*.so inside jniLibs so Android extracts
    // them executable to nativeLibraryDir. `make android` stages them.
    sourceSets {
        named("main") {
            jniLibs.srcDir("src/main/jniLibs")
        }
    }

    // Extract the bundled binaries as real files — the service execs them
    // out of nativeLibraryDir, so they must not sit compressed in the APK.
    packaging {
        jniLibs {
            useLegacyPackaging = true
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

kotlin {
    compilerOptions {
        jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17)
    }
}
