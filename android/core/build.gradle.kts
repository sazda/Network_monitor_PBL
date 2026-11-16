// Logika aplikacije bez Android-a (IMAP, pravila, ograničenje): testira se na običnoj JVM.
import org.jetbrains.kotlin.gradle.dsl.JvmTarget

plugins {
    kotlin("jvm")
}

java {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
}

kotlin {
    compilerOptions { jvmTarget.set(JvmTarget.JVM_17) }
}

dependencies {
    // javax.mail za Android (radi i na JVM-u, pa se isto koristi u testovima).
    api("com.sun.mail:android-mail:1.6.8")
    api("com.sun.mail:android-activation:1.6.8")
    testImplementation(kotlin("test"))
}

tasks.test {
    useJUnitPlatform()
}
