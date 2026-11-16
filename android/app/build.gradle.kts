import org.jetbrains.kotlin.gradle.dsl.JvmTarget

plugins {
    id("com.android.application")
    kotlin("android")
}

// Potpis: ključ se zadaje preko promenljivih okruženja (GitHub tajne), nikad
// se ne čuva u repozitorijumu. Bez ključa se pravi probni APK sa debug potpisom.
val keystorePath: String? = System.getenv("NETMON_KEYSTORE")?.takeIf { it.isNotBlank() && file(it).exists() }

android {
    namespace = "rs.netmon.sms"
    compileSdk = 35

    defaultConfig {
        applicationId = "rs.netmon.sms"
        minSdk = 26
        targetSdk = 35
        versionCode = (System.getenv("NETMON_VERSION_CODE") ?: "1").toInt()
        versionName = System.getenv("NETMON_VERSION_NAME") ?: "dev"
    }

    signingConfigs {
        if (keystorePath != null) {
            create("release") {
                storeFile = file(keystorePath)
                storePassword = System.getenv("NETMON_KEYSTORE_PASSWORD")
                keyAlias = System.getenv("NETMON_KEY_ALIAS")
                keyPassword = System.getenv("NETMON_KEY_PASSWORD")
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            signingConfig = if (keystorePath != null) signingConfigs.getByName("release") else signingConfigs.getByName("debug")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    packaging {
        resources {
            excludes += setOf("META-INF/LICENSE*", "META-INF/NOTICE*", "META-INF/*.kotlin_module")
        }
    }
}

kotlin {
    compilerOptions { jvmTarget.set(JvmTarget.JVM_17) }
}

dependencies {
    implementation(project(":core"))
}
